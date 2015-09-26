/*
 * portal - marshal
 *
 * a library that implements an algorithm for doing consumer coordination within Kafka, rather
 * than using Zookeeper or another external system.
 *
 */

package marshal

import (
	"errors"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/optiopay/kafka"
	"github.com/optiopay/kafka/proto"
)

// ConsumerBehavior is the broad category of behaviors that encapsulate how the Consumer
// will handle claiming/releasing partitions.
type ConsumerBehavior int

const (
	// CbAggressive specifies that the consumer should attempt to claim all unclaimed
	// partitions immediately. This is appropriate in low QPS situations where you are
	// mainly using this library to ensure failover to standby consumers.
	CbAggressive ConsumerBehavior = iota

	// CbBalanced ramps up more slowly than CbAggressive, but is more appropriate in
	// high QPS situations where you know that a single Consumer will never be able to
	// handle the entire topic's traffic.
	CbBalanced = iota
)

// consumerClaim is our internal tracking structure about a partition.
type consumerClaim struct {
	topic         string
	partID        int
	claimed       *int32
	lastHeartbeat int64
	consumer      kafka.Consumer
	messages      chan *proto.Message

	// Number of heartbeat cycles this claim has been lagging, i.e., consumption is going
	// too slowly (defined as being behind by more than 2 heartbeat cycles)
	cyclesBehind int

	// A Kafka partition consists of N messages with offsets. In the basic case, you
	// can think of an offset like an array index. With log compaction and other trickery
	// it acts more like a sparse array, but it's a close enough metaphor.
	//
	// We keep track of four values for offsets:
	//
	//    offsets       1     2     3     7     9    10    11
	//   partition  [ msg1, msg2, msg3, msg4, msg5, msg6, msg7, ... ]
	//                 ^                  ^                      ^
	//                 \- offsetEarliest  |                      |
	//                                    \- offsetCurrent   offsetLatest
	//                                     and startOffset
	//
	// In this example, offsetEarliest is 1 which is the "oldest" offset within the
	// partition. At any given time this offset might become invalid (if a log rolls)
	// so we might update it. startOffset is simply the value of offsetCurrent at
	// the very beginning of consumption (for tracking velocity).
	//
	// offsetCurrent is 7, which is the offset of the NEXT message i.e. this message
	// has not been consumed yet.
	//
	// offsetLatest is 12, which is the offset that Kafka will assign to the message
	// that next gets committed to the partition. This offset does not yet exist,
	// and mit might never.
	offsetCurrent  int64
	offsetEarliest int64
	offsetLatest   int64

	// These track the offset and time of when consumption started. This is going to
	// be the values from the first Heartbeat message we generate.
	startOffset int64
	startTime   int64
}

// messagePump continuously pulls message from Kafka for this partition and makes them
// available for consumption.
func (c *consumerClaim) messagePump() {
	// This method MUST NOT make changes to the consumerClaim structure. Since we might
	// be running while someone else has the lock, and we can't get it ourselves, we are
	// forbidden to touch anything other than the consumer and the message channel.
	for {
		if atomic.LoadInt32(c.claimed) != 1 {
			log.Infof("%s:%d no longer claimed, pump exiting.", c.topic, c.partID)
			return
		}

		msg, err := c.consumer.Consume()
		if err == proto.ErrOffsetOutOfRange {
			// Fell out of range, presumably because we're handling this too slow, so
			// let's abandon this consumer
			log.Errorf("%s:%d error consuming: out of range, abandoning partition",
				c.topic, c.partID)
			atomic.StoreInt32(c.claimed, 0)
			return
		}
		if err != nil {
			log.Errorf("%s:%d error consuming: %s", c.topic, c.partID, err)

			// Often a consumption error is caused by data going away, such as if we're consuming
			// from the head and Kafka has deleted the data. In that case we need to wait for
			// the next offset update, so let's not go crazy
			time.Sleep(1 * time.Second)
			continue
		}

		c.messages <- msg
	}
}

// Consumer allows you to safely consume data from a given topic in such a way that you
// don't need to worry about partitions and can safely split the load across as many
// processes as might be consuming from this topic. However, you should ONLY create one
// Consumer per topic in your application!
type Consumer struct {
	marshal    *Marshaler
	topic      string
	partitions int
	rand       *rand.Rand
	claims     map[int]*consumerClaim
	lock       sync.RWMutex
	behavior   ConsumerBehavior
}

// NewConsumer instantiates a consumer object for a given topic. You must create a
// separate consumer for every individual topic that you want to consume from. Please
// see the documentation on ConsumerBehavior.
func NewConsumer(marshal *Marshaler, topicName string,
	behavior ConsumerBehavior) (*Consumer, error) {

	if marshal == nil {
		return nil, errors.New("Must provide a marshaler")
	}

	consumer := &Consumer{
		marshal:    marshal,
		topic:      topicName,
		partitions: marshal.Partitions(topicName),
		behavior:   behavior,
		rand:       rand.New(rand.NewSource(time.Now().UnixNano())),
		claims:     make(map[int]*consumerClaim),
	}
	go consumer.manageClaims()

	return consumer, nil
}

// updateOffsets will update the offsets of any partitions that we claim.
func (c *Consumer) updateOffsets() error {
	c.lock.Lock()
	defer c.lock.Unlock()

	for partID, claim := range c.claims {
		oEarly, oLate, _, err := c.marshal.GetPartitionOffsets(c.topic, partID)
		if err != nil {
			log.Errorf("Failed to get offsets for %s:%d: %s", c.topic, partID, err)
			return err
		}

		// DO NOT update current, but we can update early/late
		claim.offsetEarliest = oEarly
		claim.offsetLatest = oLate
	}
	return nil
}

// tryClaimPartition attempts to claim a partition and make it available in the consumption
// flow. If this is called a second time on a partition we already own, it will return
// false. Returns true only if the partition was never claimed and we succeeded in
// claiming it.
func (c *Consumer) tryClaimPartition(partID int) bool {
	// Partition unclaimed by us, see if it's claimed by anybody
	// TODO: This is where we probably want to insert "claim reassertion" logic? I.e.,
	// if the clientid/groupid are the same, here is where we would promote the claim
	// back into our own structure so the consumer would just pick up where it left off
	currentClaim := c.marshal.GetPartitionClaim(c.topic, partID)
	if currentClaim.LastHeartbeat > 0 {
		return false
	}

	// Get all available offset information
	oEarly, oLate, oCur, err := c.marshal.GetPartitionOffsets(c.topic, partID)
	if err != nil {
		log.Errorf("Failed to get offsets for %s:%d: %s", c.topic, partID, err)
		return false
	}
	log.Debugf("Partition %s:%d offsets: early = %d, cur = %d, late = %d",
		c.topic, partID, oEarly, oCur, oLate)

	// Set up internal claim structure we'll track things in
	claim := &consumerClaim{
		topic:          c.topic,
		partID:         partID,
		claimed:        new(int32),
		offsetEarliest: oEarly,
		offsetLatest:   oLate,
		offsetCurrent:  oCur,
		messages:       make(chan *proto.Message, 100),
	}
	atomic.StoreInt32(claim.claimed, 1)

	// Now try to actually claim it, this can block a while
	log.Infof("Consumer attempting to claim: %s:%d", c.topic, partID)
	if !c.marshal.ClaimPartition(c.topic, partID) {
		log.Infof("Consumer failed to claim: %s:%d", c.topic, partID)
		return false
	}

	// Of course, if the current offset is greater than the earliest, we must reset
	// to the earliest known
	if claim.offsetCurrent < claim.offsetEarliest {
		log.Warningf("Consumer fast-forwarding %s:%d: from %d to %d",
			c.topic, partID, claim.offsetCurrent, claim.offsetEarliest)
		claim.offsetCurrent = claim.offsetEarliest
	}

	// Since it's claimed, we now want to heartbeat with the last seen offset
	err = c.marshal.Heartbeat(c.topic, partID, claim.offsetCurrent)
	if err != nil {
		log.Errorf("Consumer failed to heartbeat %s:%d: %s", c.topic, partID, err)
		return false
	}
	claim.lastHeartbeat = time.Now().Unix()

	// Set the starting position so we can calculate velocity later
	claim.startOffset = claim.offsetCurrent
	claim.startTime = claim.lastHeartbeat

	// Set up Kafka consumer
	consumerConf := kafka.NewConsumerConf(c.topic, int32(partID))
	consumerConf.StartOffset = claim.offsetCurrent
	kafkaConsumer, err := c.marshal.kafka.Consumer(consumerConf)
	if err != nil {
		log.Errorf("Consumer failed to create Kafka Consumer: %s:%d got %s",
			c.topic, partID, err)
		// TODO: There is an optimization here where we could release the partition.
		// As it stands, we're not doing anything,
		return false
	}
	claim.consumer = kafkaConsumer

	// Now start consuming from this partition (as long as we haven't terminated)
	go claim.messagePump()

	// Totally done, update our internal structures
	log.Infof("Consumer claimed: %s:%d at offset %d (is %d behind)",
		c.topic, partID, claim.offsetCurrent, claim.offsetLatest)

	// Finally overwrite our structure pointer (state is committed to ourselves)
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.claims != nil {
		// Can be nil when we're terminating
		c.claims[partID] = claim
	}
	return true
}

// claimPartitions actually attempts to claim partitions. If the current consumer is
// set on aggressive, this will try to claim ALL partitions that are free. Balanced mode
// will claim a single partition.
func (c *Consumer) claimPartitions() {
	offset := rand.Intn(c.partitions)
	for i := 0; i < c.partitions; i++ {
		partID := (i + offset) % c.partitions

		// If it's present in the structure, we assert that it's claimed by us
		c.lock.RLock()
		if _, ok := c.claims[partID]; ok {
			c.lock.RUnlock()
			continue
		}
		c.lock.RUnlock()

		if !c.tryClaimPartition(partID) {
			continue
		}

		// Balanced mode means we abort our claim now, since we've got one, whereas
		// aggressive claims as many as it can
		if c.behavior == CbBalanced {
			break
		}
	}
}

func (c *Consumer) getUnhealthyClaims() []*consumerClaim {
	nowTime := time.Now().Unix()

	c.lock.Lock()
	defer c.lock.Unlock()

	var unclaimPartitions []*consumerClaim
	for _, claim := range c.claims {
		// If a partition has unmarked itself then it has determined that it is unhealthy,
		// so we want to unclaim it
		if atomic.LoadInt32(claim.claimed) == 0 {
			unclaimPartitions = append(unclaimPartitions, claim)
			continue
		}

		// If current has gone forward of the latest (which is possible, but unlikely)
		// then we are by definition caught up
		if claim.offsetCurrent >= claim.offsetLatest {
			continue
		}

		// And if we've never consumed from the partition, then never let it be considered
		// unhealthy (by this flow). A partition that is not being consumed from is not
		// being heartbeated to, so we will lose the lock through that path.
		if claim.offsetCurrent == claim.startOffset || nowTime == claim.startTime {
			continue
		}

		// Calculate the velocity (how fast the consumer is going) and how many seconds
		// behind we seem to be
		velocity := float64(claim.offsetCurrent-claim.startOffset) /
			float64(nowTime-claim.startTime)
		secondsBehind := float64(claim.offsetLatest-claim.offsetCurrent) / velocity

		// If it's over two intervals, increase the cycles behind counter (we don't want
		// to oscillate claims too much, and we need to ensure we have enough data to
		// decide it's unhealthy).
		if secondsBehind > HeartbeatInterval*2 {
			claim.cyclesBehind++
			log.Warningf("Consumer for %s:%d is %0.2f seconds behind, %d cycle(s).",
				claim.topic, claim.partID, secondsBehind, claim.cyclesBehind)

			// If were behind by too many cycles, then we should try to release the
			// partition.
			if claim.cyclesBehind >= 3 {
				unclaimPartitions = append(unclaimPartitions, claim)
			}
		} else {
			claim.cyclesBehind = 0
		}
	}

	return unclaimPartitions
}

// manageClaims is our internal state machine that handles partitions and claiming new
// ones (or releasing ones).
func (c *Consumer) manageClaims() {
	nextOffsetUpdate := time.Now()
	for {
		// See if consumer is alive (hasn't been terminated)
		c.lock.RLock()
		if c.claims == nil {
			c.lock.RUnlock()
			return
		}
		claimCount := len(c.claims)
		c.lock.RUnlock()

		// Update offsets of all of our claims so we can check how far along they are
		if time.Now().After(nextOffsetUpdate) {
			c.updateOffsets()
			nextOffsetUpdate = time.Now().Add(HeartbeatInterval * time.Second)

			// Get a list of partitions that seem to be unhealthy (too far behind)
			unclaimPartitions := c.getUnhealthyClaims()

			// If any partitions to unclaim, do it
			if len(unclaimPartitions) > 0 {
				// At most, we're only willing to release up to half of our partitions, because we
				// don't want to end up giving up everything
				maxToRelease := claimCount / 2
				log.Warningf("Found %d unhealthy partitions out of %d total.",
					len(unclaimPartitions), claimCount)

				// Kill the message pumps, unclaim them internally, and delete from the list
				c.lock.Lock()
				for _, claim := range unclaimPartitions {
					if maxToRelease <= 0 {
						log.Warningf("Too many partitions unhealthy, keeping some.")
						break
					}
					maxToRelease--

					atomic.StoreInt32(claim.claimed, 0)
					delete(c.claims, claim.partID)
				}
				c.lock.Unlock()

				// Now that we're outside of the lock, actually do the slow production of the
				// release partition messages
				for _, claim := range unclaimPartitions {
					log.Warningf("Releasing %s:%d: consumer is unhealthy.",
						claim.topic, claim.partID)
					c.marshal.ReleasePartition(claim.topic, claim.partID, claim.offsetCurrent)
				}
			}
		}

		// At this point, we don't need to engage in any load shedding behavior, so let's
		// see if there are any partitions out there we can claim
		c.claimPartitions()

		// Now sleep a bit so we don't pound things
		time.Sleep(time.Duration(rand.Intn(1000)) * time.Millisecond)
	}
}

// Terminate instructs the consumer to release its locks. This will allow other consumers
// to begin consuming. (If you do not call this method before exiting, things will still
// work, but more slowly.)
func (c *Consumer) Terminate() {
	c.lock.Lock()
	defer c.lock.Unlock()

	for partID, claim := range c.claims {
		atomic.StoreInt32(claim.claimed, 0) // Terminates the pump.
		err := c.marshal.ReleasePartition(c.topic, partID, claim.offsetCurrent)
		if err == nil {
			log.Infof("Consumer termination: released %s:%d at %d",
				c.topic, partID, claim.offsetCurrent)
		} else {
			log.Errorf("Consumer termination: failed to release %s:%d: %s",
				c.topic, partID, err)
		}
	}

	// Remove the map, so we can't operate anymore
	c.claims = nil
}

// GetCurrentLag returns the number of messages that this consumer is lagging by. Note that
// this value can be unstable in the beginning of a run, as we might not have claimed all of
// partitions we will end up claiming, or we might have overclaimed and need to back off.
// Ideally this will settle towards 0. If it continues to rise, that implies there isn't
// enough consumer capacity.
func (c *Consumer) GetCurrentLag() int {
	c.lock.RLock()
	defer c.lock.RUnlock()

	var lag int
	for _, claim := range c.claims {
		if claim.offsetLatest > claim.offsetCurrent {
			lag += int(claim.offsetLatest - claim.offsetCurrent)
		}
	}
	return lag
}

// GetCurrentLoad returns a number representing the "load" of this consumer. Think of this
// like a load average in Unix systems: the numbers are kind of related to how much work
// the system is doing, but by itself they don't tell you much.
func (c *Consumer) GetCurrentLoad() int {
	c.lock.RLock()
	defer c.lock.RUnlock()

	return len(c.claims)
}

// Consume returns the next available message from the topic. If no messages are available,
// it will block until one is.
func (c *Consumer) Consume() []byte {
	// TODO: This is almost certainly a slow implementation as we have to scan everything
	// every time.
	for {
		var msg *proto.Message

		// TODO: This implementation also can lead to queue starvation since we start at the
		// front every time.
		// TODO: Rethink this locking. It really is confusing...
		c.lock.RLock()
		for _, claim := range c.claims {
			select {
			case msg = <-claim.messages:
				break
			default:
				// Do nothing.
			}
		}
		c.lock.RUnlock()

		// TODO: This is braindead.
		if msg == nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}

		// Once we're consuming a message we need to update our data structure as well
		// as possibly make a heartbeat for this partition so the world knows we're
		// still actively consuming
		// TODO: There is a trap here, this is actually probably a bad design in that
		// the consumer has its own idea of "claim" which in theory mirrors what the
		// marshaler has... but there's no guarantee
		// Of course for the ALO consumer, that's probably OK. It's just a bad code design
		// I think.
		// Actually it might not be that bad. Only the consumer knows when it has decided
		// it has fallen too far behind and released a partition.

		c.lock.Lock()
		claim, ok := c.claims[int(msg.Partition)]
		if !ok {
			c.lock.Unlock()
			continue
		}
		if atomic.LoadInt32(claim.claimed) == 1 {
			// Since we've consumed the message at Offset, our cursor now points to the
			// next message
			claim.offsetCurrent = msg.Offset + 1

			// Possibly heartbeat
			now := time.Now().Unix()
			if claim.lastHeartbeat <= (now - HeartbeatInterval) {
				claim.lastHeartbeat = now
				// Do this in a goroutine so as not to block the consumption
				go func() {
					// Don't use 'claim' here, as this will definitely run outside of the lock
					log.Debugf("Need to heartbeat for %s:%d.", msg.Topic, msg.Partition)
					c.marshal.Heartbeat(msg.Topic, int(msg.Partition), msg.Offset)
				}()
			}
			c.lock.Unlock()
			return msg.Value
		}
		c.lock.Unlock()

		// If we get here, then we retrieved a message for a partition that is not
		// presently claimed, so let's just pretend we didn't and continue back to the top
		// for the next message
	}
}