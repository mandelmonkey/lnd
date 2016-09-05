package channeldb

import (
	"bytes"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/boltdb/bolt"
	"github.com/lightningnetwork/lnd/elkrem"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

var (
	// openChanBucket stores all the currently open channels. This bucket
	// has a second, nested bucket which is keyed by a node's ID. Additionally,
	// at the base level of this bucket several prefixed keys are stored which
	// house channel meta-data such as total satoshis sent, number of updates
	// etc. These fields are stored at this top level rather than within a
	// node's channel bucket in orer to facilitate sequential prefix scans
	// to gather stats such as total satoshis received.
	openChannelBucket = []byte("ocb")

	// chanIDBucket is a thrid-level bucket stored within a node's ID bucket
	// in the open channel bucket. The resolution path looks something like:
	// ocb -> nodeID -> cib. This bucket contains a series of keys with no
	// values, these keys are the channel ID's of all the active channels
	// we currently have with a specified nodeID. This bucket acts as an
	// additional indexing allowing random access and sequential scans over
	// active channels.
	chanIDBucket = []byte("cib")

	// closedChannelBucket stores summarization information concerning
	// previously open, but now closed channels.
	closedChannelBucket = []byte("ccb")

	// channelLogBucket is dedicated for storing the necessary delta state
	// between channel updates required to re-construct a past state in
	// order to punish a counter party attempting a non-cooperative channel
	// closure.
	channelLogBucket = []byte("clb")

	// identityKey is the key for storing this node's current LD identity key.
	identityKey = []byte("idk")

	// The following prefixes are stored at the base level within the
	// openChannelBucket. In order to retrieve a particular field for an
	// active, or historic channel, append the channels ID to the prefix:
	// key = prefix || chanID. Storing certain fields at the top level
	// using a prefix scheme serves two purposes: first to facilitate
	// sequential prefix scans, and second to eliminate write amplification
	// caused by serializing/deserializing the *entire* struct with each
	// update.
	chanCapacityPrefix = []byte("ccp")
	selfBalancePrefix  = []byte("sbp")
	theirBalancePrefix = []byte("tbp")
	minFeePerKbPrefix  = []byte("mfp")
	updatePrefix       = []byte("uup")
	satSentPrefix      = []byte("ssp")
	satRecievedPrefix  = []byte("srp")
	netFeesPrefix      = []byte("ntp")

	// chanIDKey stores the node, and channelID for an active channel.
	chanIDKey = []byte("cik")

	// commitKeys stores both commitment keys (ours, and theirs) for an
	// active channel. Our private key is stored in an encrypted format
	// using channeldb's currently registered cryptoSystem.
	commitKeys = []byte("ckk")

	// commitTxnsKey stores the full version of both current, non-revoked
	// commitment transactions in addition to the csvDelay for both.
	commitTxnsKey = []byte("ctk")

	// fundingTxnKey stroes the funding tx, our encrypted multi-sig key,
	// and finally 2-of-2 multisig redeem script.
	fundingTxnKey = []byte("fsk")

	// elkremStateKey stores their current revocation hash, and our elkrem
	// sender, and their elkrem reciever.
	elkremStateKey = []byte("esk")

	// deliveryScriptsKey stores the scripts for the final delivery in the
	// case of a cooperative closure.
	deliveryScriptsKey = []byte("dsk")
)

// OpenChannel encapsulates the persistent and dynamic state of an open channel
// with a remote node. An open channel supports several options for on-disk
// serialization depending on the exact context. Full (upon channel creation)
// state commitments, and partial (due to a commitment update) writes are
// supported. Each partial write due to a state update appends the new update
// to an on-disk log, which can then subsequently be queried in order to
// "time-travel" to a prior state.
type OpenChannel struct {
	// Hash? or Their current pubKey?
	TheirLNID [wire.HashSize]byte

	// The ID of a channel is the txid of the funding transaction.
	ChanID *wire.OutPoint

	MinFeePerKb btcutil.Amount
	// Our reserve. Assume symmetric reserve amounts. Only needed if the
	// funding type is CLTV.
	//ReserveAmount btcutil.Amount

	// Keys for both sides to be used for the commitment transactions.
	OurCommitKey   *btcec.PublicKey
	TheirCommitKey *btcec.PublicKey

	// Tracking total channel capacity, and the amount of funds allocated
	// to each side.
	Capacity     btcutil.Amount
	OurBalance   btcutil.Amount
	TheirBalance btcutil.Amount

	// Our current commitment transaction along with their signature for
	// our commitment transaction.
	OurCommitTx  *wire.MsgTx
	OurCommitSig []byte

	// The outpoint of the final funding transaction.
	FundingOutpoint *wire.OutPoint

	OurMultiSigKey      *btcec.PublicKey
	TheirMultiSigKey    *btcec.PublicKey
	FundingRedeemScript []byte

	// In blocks
	LocalCsvDelay  uint32
	RemoteCsvDelay uint32

	// Current revocation for their commitment transaction. However, since
	// this the derived public key, we don't yet have the pre-image so we
	// aren't yet able to verify that it's actually in the hash chain.
	TheirCurrentRevocation     *btcec.PublicKey
	TheirCurrentRevocationHash [32]byte
	LocalElkrem                *elkrem.ElkremSender
	RemoteElkrem               *elkrem.ElkremReceiver

	// The pkScript for both sides to be used for final delivery in the case
	// of a cooperative close.
	OurDeliveryScript   []byte
	TheirDeliveryScript []byte

	NumUpdates            uint64
	TotalSatoshisSent     uint64
	TotalSatoshisReceived uint64
	TotalNetFees          uint64    // TODO(roasbeef): total fees paid too?
	CreationTime          time.Time // TODO(roasbeef): last update time?

	// TODO(roasbeef): eww
	Db *DB

	sync.RWMutex
}

// FullSync serializes, and writes to disk the *full* channel state, using
// both the active channel bucket to store the prefixed column fields, and the
// remote node's ID to store the remainder of the channel state.
//
// NOTE: This method requires an active EncryptorDecryptor to be registered in
// order to encrypt sensitive information.
func (c *OpenChannel) FullSync() error {
	return c.Db.store.Update(func(tx *bolt.Tx) error {
		// TODO(roasbeef): add helper funcs to create scoped update
		// First fetch the top level bucket which stores all data related to
		// current, active channels.
		chanBucket, err := tx.CreateBucketIfNotExists(openChannelBucket)
		if err != nil {
			return err
		}

		// Within this top level bucket, fetch the bucket dedicated to storing
		// open channel data specific to the remote node.
		nodeChanBucket, err := chanBucket.CreateBucketIfNotExists(c.TheirLNID[:])
		if err != nil {
			return err
		}

		// Add this channel ID to the node's active channel index if
		// it doesn't already exist.
		chanIDBucket, err := nodeChanBucket.CreateBucketIfNotExists(chanIDBucket)
		if err != nil {
			return err
		}
		var b bytes.Buffer
		if err := writeOutpoint(&b, c.ChanID); err != nil {
			return err
		}
		if chanIDBucket.Get(b.Bytes()) == nil {
			chanIDBucket.Put(b.Bytes(), nil)
		}

		return putOpenChannel(chanBucket, nodeChanBucket, c)
	})
}

// SyncRevocation writes to disk the current revocation state of the channel.
// The revocation state is defined as the current elkrem receiver, and the
// latest unrevoked key+hash for the remote party.
func (c *OpenChannel) SyncRevocation() error {
	return c.Db.store.Update(func(tx *bolt.Tx) error {
		// First fetch the top level bucket which stores all data related to
		// current, active channels.
		chanBucket, err := tx.CreateBucketIfNotExists(openChannelBucket)
		if err != nil {
			return err
		}

		// Within this top level bucket, fetch the bucket dedicated to storing
		// open channel data specific to the remote node.
		nodeChanBucket, err := chanBucket.CreateBucketIfNotExists(c.TheirLNID[:])
		if err != nil {
			return err
		}

		// Sync the current elkrem state to disk.
		if err := putChanEklremState(nodeChanBucket, c); err != nil {
			return err
		}

		return nil
	})
}

// HTLC is the on-disk representation of a hash time-locked contract. HTLC's
// are contained within ChannelDeltas which encode the current state of the
// commitment between state updates.
type HTLC struct {
	// Incoming denotes whether we're the receiver or the sender of this
	// HTLC.
	Incoming bool

	// Amt is the amount of satoshis this HTLC escrows.
	Amt btcutil.Amount

	// RHash is the payment hash of the HTLC.
	RHash [32]byte

	// RefundTimeout is the absolute timeout on the HTLC that the sender
	// must wait before reclaiming the funds in limbo.
	RefundTimeout uint32

	// RevocationTimeout is the relative timeout the party who broadcasts
	// the commitment transaction must wait before being able to fully
	// sweep the funds on-chain in the case of a unilateral channel
	// closure.
	RevocationTimeout uint32
}

// ChannelDelta is a snapshot of the commitment state at a particular point in
// the commitment chain. With each state transition, a snapshot of the current
// state along with all non-settled HTLC's are recorded.
type ChannelDelta struct {
	LocalBalance  btcutil.Amount
	RemoteBalance btcutil.Amount
	UpdateNum     uint32

	Htlcs []*HTLC
}

// RecordChannelDelta records the new state transition within an on-disk
// append-only log which records all state transitions. Additionally, the
// internal balances and update counter of the target OpenChannel are updated
// accordingly based on the passed delta.
func (c *OpenChannel) RecordChannelDelta(newCommitment *wire.MsgTx,
	newSig []byte, delta *ChannelDelta) error {

	return c.Db.store.Update(func(tx *bolt.Tx) error {
		chanBucket, err := tx.CreateBucketIfNotExists(openChannelBucket)
		if err != nil {
			return err
		}

		id := c.TheirLNID[:]
		nodeChanBucket, err := chanBucket.CreateBucketIfNotExists(id)
		if nodeChanBucket == nil {
			return ErrNoActiveChannels
		}

		// TODO(roasbeef): revisit in-line mutation
		c.OurCommitTx = newCommitment
		c.OurBalance = delta.LocalBalance
		c.TheirBalance = delta.RemoteBalance
		c.OurCommitSig = newSig
		c.NumUpdates = uint64(delta.UpdateNum)

		// First we'll write out the current latest dynamic channel
		// state: the current channel balance, the number of updates,
		// and our latest commitment transaction+sig.
		if err := putChanCapacity(chanBucket, c); err != nil {
			return err
		}
		if err := putChanNumUpdates(chanBucket, c); err != nil {
			return err
		}
		if err := putChanCommitTxns(nodeChanBucket, c); err != nil {
			return err
		}

		// With the current state updated, append a new log entry
		// recording this the delta of this state transition.
		// TODO(roasbeef): could make the deltas relative, would save
		// space, but then tradeoff for more disk-seeks to recover the
		// full state.
		logKey := channelLogBucket
		logBucket, err := nodeChanBucket.CreateBucketIfNotExists(logKey)
		if err != nil {
			return err
		}

		return appendChannelLogEntry(logBucket, delta, c.ChanID)
	})
}

// FindPreviousState scans through the append-only log in an attempt to recover
// the previous channel state indicated by the update number. This method is
// intended to be used for obtaining the relevant data needed to claim all
// funds rightfully spendable in the case of an on-chain broadcast of the
// commitment transaction.
func (c *OpenChannel) FindPreviousState(updateNum uint64) (*ChannelDelta, error) {
	delta := &ChannelDelta{}

	err := c.Db.store.View(func(tx *bolt.Tx) error {
		chanBucket := tx.Bucket(openChannelBucket)

		nodeChanBucket := chanBucket.Bucket(c.TheirLNID[:])
		if nodeChanBucket == nil {
			return ErrNoActiveChannels
		}

		logBucket := nodeChanBucket.Bucket(channelLogBucket)
		if nodeChanBucket == nil {
			return ErrNoPastDeltas
		}

		var err error
		delta, err = fetchChannelLogEntry(logBucket, c.ChanID,
			uint32(updateNum))

		return err
	})
	if err != nil {
		return nil, err
	}

	return delta, nil
}

// CloseChannel closes a previously active lightning channel. Closing a channel
// entails deleting all saved state within the database concerning this
// channel, as well as created a small channel summary for record keeping
// purposes.
func (c *OpenChannel) CloseChannel() error {
	return c.Db.store.Update(func(tx *bolt.Tx) error {
		// First fetch the top level bucket which stores all data related to
		// current, active channels.
		chanBucket := tx.Bucket(openChannelBucket)
		if chanBucket == nil {
			return ErrNoChanDBExists
		}

		// Within this top level bucket, fetch the bucket dedicated to storing
		// open channel data specific to the remote node.
		nodeChanBucket := chanBucket.Bucket(c.TheirLNID[:])
		if nodeChanBucket == nil {
			return ErrNoActiveChannels
		}

		// Delete this channel ID from the node's active channel index.
		chanIndexBucket := nodeChanBucket.Bucket(chanIDBucket)
		if chanIndexBucket == nil {
			return ErrNoActiveChannels
		}
		var b bytes.Buffer
		if err := writeOutpoint(&b, c.ChanID); err != nil {
			return err
		}
		outPointBytes := b.Bytes()
		if err := chanIndexBucket.Delete(b.Bytes()); err != nil {
			return err
		}

		// Now that the index to this channel has been deleted, purge
		// the remaining channel meta-data from the database.
		if err := deleteOpenChannel(chanBucket, nodeChanBucket,
			outPointBytes); err != nil {
			return err
		}

		// Finally, create a summary of this channel in the closed
		// channel bucket for this node.
		return putClosedChannelSummary(tx, outPointBytes)
	})
}

// ChannelSnapshot is a frozen snapshot of the current channel state. A
// snapshot is detached from the original channel that generated it, providing
// read-only access to the current or prior state of an active channel.
type ChannelSnapshot struct {
	RemoteID [wire.HashSize]byte

	ChannelPoint *wire.OutPoint

	Capacity      btcutil.Amount
	LocalBalance  btcutil.Amount
	RemoteBalance btcutil.Amount

	NumUpdates uint64

	TotalSatoshisSent     uint64
	TotalSatoshisReceived uint64

	// TODO(roasbeef): fee stuff
	updateNum uint64
	channel   *OpenChannel
}

// Snapshot returns a read-only snapshot of the current channel state. This
// snapshot includes information concerning the current settled balance within
// the channel, meta-data detailing total flows, and any outstanding HTLCs.
func (c *OpenChannel) Snapshot() *ChannelSnapshot {
	snapshot := &ChannelSnapshot{
		ChannelPoint:          c.ChanID,
		Capacity:              c.Capacity,
		LocalBalance:          c.OurBalance,
		RemoteBalance:         c.TheirBalance,
		NumUpdates:            c.NumUpdates,
		TotalSatoshisSent:     c.TotalSatoshisSent,
		TotalSatoshisReceived: c.TotalSatoshisReceived,
	}
	copy(snapshot.RemoteID[:], c.TheirLNID[:])

	// TODO(roasbeef): cache current channel delta in memory, either merge
	// or replace with ChannelSnapshot

	return snapshot
}

func putClosedChannelSummary(tx *bolt.Tx, chanID []byte) error {
	// For now, a summary of a closed channel simply involves recording the
	// outpoint of the funding transaction.
	closedChanBucket, err := tx.CreateBucketIfNotExists(closedChannelBucket)
	if err != nil {
		return err
	}

	// TODO(roasbeef): add other info
	//  * should likely have each in own bucket per node
	return closedChanBucket.Put(chanID, nil)
}

// putChannel serializes, and stores the current state of the channel in its
// entirety.
func putOpenChannel(openChanBucket *bolt.Bucket, nodeChanBucket *bolt.Bucket,
	channel *OpenChannel) error {

	// First write out all the "common" fields using the field's prefix
	// appened with the channel's ID. These fields go into a top-level bucket
	// to allow for ease of metric aggregation via efficient prefix scans.
	if err := putChanCapacity(openChanBucket, channel); err != nil {
		return err
	}
	if err := putChanMinFeePerKb(openChanBucket, channel); err != nil {
		return err
	}
	if err := putChanNumUpdates(openChanBucket, channel); err != nil {
		return err
	}
	if err := putChanTotalFlow(openChanBucket, channel); err != nil {
		return err
	}
	if err := putChanNetFee(openChanBucket, channel); err != nil {
		return err
	}

	// Next, write out the fields of the channel update less frequently.
	if err := putChannelIDs(nodeChanBucket, channel); err != nil {
		return err
	}
	if err := putChanCommitKeys(nodeChanBucket, channel); err != nil {
		return err
	}
	if err := putChanCommitTxns(nodeChanBucket, channel); err != nil {
		return err
	}
	if err := putChanFundingInfo(nodeChanBucket, channel); err != nil {
		return err
	}
	if err := putChanEklremState(nodeChanBucket, channel); err != nil {
		return err
	}
	if err := putChanDeliveryScripts(nodeChanBucket, channel); err != nil {
		return err
	}

	return nil
}

// fetchOpenChannel retrieves, and deserializes (including decrypting
// sensitive) the complete channel currently active with the passed nodeID.
// An EncryptorDecryptor is required to decrypt sensitive information stored
// within the database.
func fetchOpenChannel(openChanBucket *bolt.Bucket, nodeChanBucket *bolt.Bucket,
	chanID *wire.OutPoint) (*OpenChannel, error) {

	channel := &OpenChannel{
		ChanID: chanID,
	}

	// First, read out the fields of the channel update less frequently.
	if err := fetchChannelIDs(nodeChanBucket, channel); err != nil {
		return nil, err
	}
	if err := fetchChanCommitKeys(nodeChanBucket, channel); err != nil {
		return nil, err
	}
	if err := fetchChanCommitTxns(nodeChanBucket, channel); err != nil {
		return nil, err
	}
	if err := fetchChanFundingInfo(nodeChanBucket, channel); err != nil {
		return nil, err
	}
	if err := fetchChanEklremState(nodeChanBucket, channel); err != nil {
		return nil, err
	}
	if err := fetchChanDeliveryScripts(nodeChanBucket, channel); err != nil {
		return nil, err
	}

	// With the existence of an open channel bucket with this node verified,
	// perform a full read of the entire struct. Starting with the prefixed
	// fields residing in the parent bucket.
	if err := fetchChanCapacity(openChanBucket, channel); err != nil {
		return nil, err
	}
	if err := fetchChanMinFeePerKb(openChanBucket, channel); err != nil {
		return nil, err
	}
	if err := fetchChanNumUpdates(openChanBucket, channel); err != nil {
		return nil, err
	}
	if err := fetchChanTotalFlow(openChanBucket, channel); err != nil {
		return nil, err
	}
	if err := fetchChanNetFee(openChanBucket, channel); err != nil {
		return nil, err
	}

	return channel, nil
}

func deleteOpenChannel(openChanBucket *bolt.Bucket, nodeChanBucket *bolt.Bucket,
	channelID []byte) error {

	// First we'll delete all the "common" top level items stored outside
	// the node's channel bucket.
	if err := deleteChanCapacity(openChanBucket, channelID); err != nil {
		return err
	}
	if err := deleteChanMinFeePerKb(openChanBucket, channelID); err != nil {
		return err
	}
	if err := deleteChanNumUpdates(openChanBucket, channelID); err != nil {
		return err
	}
	if err := deleteChanTotalFlow(openChanBucket, channelID); err != nil {
		return err
	}
	if err := deleteChanNetFee(openChanBucket, channelID); err != nil {
		return err
	}

	// Finally, delete all the fields directly within the node's channel
	// bucket.
	if err := deleteChannelIDs(nodeChanBucket, channelID); err != nil {
		return err
	}
	if err := deleteChanCommitKeys(nodeChanBucket, channelID); err != nil {
		return err
	}
	if err := deleteChanCommitTxns(nodeChanBucket, channelID); err != nil {
		return err
	}
	if err := deleteChanFundingInfo(nodeChanBucket, channelID); err != nil {
		return err
	}
	if err := deleteChanEklremState(nodeChanBucket, channelID); err != nil {
		return err
	}
	if err := deleteChanDeliveryScripts(nodeChanBucket, channelID); err != nil {
		return err
	}

	return nil
}

func putChanCapacity(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	// Some scratch bytes re-used for serializing each of the uint64's.
	scratch1 := make([]byte, 8)
	scratch2 := make([]byte, 8)
	scratch3 := make([]byte, 8)

	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix[3:], b.Bytes())

	copy(keyPrefix[:3], chanCapacityPrefix)
	byteOrder.PutUint64(scratch1, uint64(channel.Capacity))
	if err := openChanBucket.Put(keyPrefix, scratch1); err != nil {
		return err
	}

	copy(keyPrefix[:3], selfBalancePrefix)
	byteOrder.PutUint64(scratch2, uint64(channel.OurBalance))
	if err := openChanBucket.Put(keyPrefix, scratch2); err != nil {
		return err
	}

	copy(keyPrefix[:3], theirBalancePrefix)
	byteOrder.PutUint64(scratch3, uint64(channel.TheirBalance))
	return openChanBucket.Put(keyPrefix, scratch3)
}

func deleteChanCapacity(openChanBucket *bolt.Bucket, chanID []byte) error {
	keyPrefix := make([]byte, 3+len(chanID))
	copy(keyPrefix[3:], chanID)

	copy(keyPrefix[:3], chanCapacityPrefix)
	if err := openChanBucket.Delete(keyPrefix); err != nil {
		return err
	}

	copy(keyPrefix[:3], selfBalancePrefix)
	if err := openChanBucket.Delete(keyPrefix); err != nil {
		return err
	}

	copy(keyPrefix[:3], theirBalancePrefix)
	return openChanBucket.Delete(keyPrefix)
}

func fetchChanCapacity(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	// A byte slice re-used to compute each key prefix below.
	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix[3:], b.Bytes())

	copy(keyPrefix[:3], chanCapacityPrefix)
	capacityBytes := openChanBucket.Get(keyPrefix)
	channel.Capacity = btcutil.Amount(byteOrder.Uint64(capacityBytes))

	copy(keyPrefix[:3], selfBalancePrefix)
	selfBalanceBytes := openChanBucket.Get(keyPrefix)
	channel.OurBalance = btcutil.Amount(byteOrder.Uint64(selfBalanceBytes))

	copy(keyPrefix[:3], theirBalancePrefix)
	theirBalanceBytes := openChanBucket.Get(keyPrefix)
	channel.TheirBalance = btcutil.Amount(byteOrder.Uint64(theirBalanceBytes))

	return nil
}

func putChanMinFeePerKb(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	scratch := make([]byte, 8)
	byteOrder.PutUint64(scratch, uint64(channel.MinFeePerKb))

	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix, minFeePerKbPrefix)
	copy(keyPrefix[3:], b.Bytes())

	return openChanBucket.Put(keyPrefix, scratch)
}

func deleteChanMinFeePerKb(openChanBucket *bolt.Bucket, chanID []byte) error {
	keyPrefix := make([]byte, 3+len(chanID))
	copy(keyPrefix, minFeePerKbPrefix)
	copy(keyPrefix[3:], chanID)
	return openChanBucket.Delete(keyPrefix)
}

func fetchChanMinFeePerKb(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix, minFeePerKbPrefix)
	copy(keyPrefix[3:], b.Bytes())

	feeBytes := openChanBucket.Get(keyPrefix)
	channel.MinFeePerKb = btcutil.Amount(byteOrder.Uint64(feeBytes))

	return nil
}

func putChanNumUpdates(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	scratch := make([]byte, 8)
	byteOrder.PutUint64(scratch, channel.NumUpdates)

	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix, updatePrefix)
	copy(keyPrefix[3:], b.Bytes())

	return openChanBucket.Put(keyPrefix, scratch)
}

func deleteChanNumUpdates(openChanBucket *bolt.Bucket, chanID []byte) error {
	keyPrefix := make([]byte, 3+len(chanID))
	copy(keyPrefix, updatePrefix)
	copy(keyPrefix[3:], chanID)
	return openChanBucket.Delete(keyPrefix)
}

func fetchChanNumUpdates(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix, updatePrefix)
	copy(keyPrefix[3:], b.Bytes())

	updateBytes := openChanBucket.Get(keyPrefix)
	channel.NumUpdates = byteOrder.Uint64(updateBytes)

	return nil
}

func putChanTotalFlow(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	scratch1 := make([]byte, 8)
	scratch2 := make([]byte, 8)

	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix[3:], b.Bytes())

	copy(keyPrefix[:3], satSentPrefix)
	byteOrder.PutUint64(scratch1, uint64(channel.TotalSatoshisSent))
	if err := openChanBucket.Put(keyPrefix, scratch1); err != nil {
		return err
	}

	copy(keyPrefix[:3], satRecievedPrefix)
	byteOrder.PutUint64(scratch2, uint64(channel.TotalSatoshisReceived))
	return openChanBucket.Put(keyPrefix, scratch2)
}

func deleteChanTotalFlow(openChanBucket *bolt.Bucket, chanID []byte) error {
	keyPrefix := make([]byte, 3+len(chanID))
	copy(keyPrefix[3:], chanID)

	copy(keyPrefix[:3], satSentPrefix)
	if err := openChanBucket.Delete(keyPrefix); err != nil {
		return err
	}

	copy(keyPrefix[:3], satRecievedPrefix)
	return openChanBucket.Delete(keyPrefix)
}

func fetchChanTotalFlow(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix[3:], b.Bytes())

	copy(keyPrefix[:3], satSentPrefix)
	totalSentBytes := openChanBucket.Get(keyPrefix)
	channel.TotalSatoshisSent = byteOrder.Uint64(totalSentBytes)

	copy(keyPrefix[:3], satRecievedPrefix)
	totalReceivedBytes := openChanBucket.Get(keyPrefix)
	channel.TotalSatoshisReceived = byteOrder.Uint64(totalReceivedBytes)

	return nil
}

func putChanNetFee(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	scratch := make([]byte, 8)

	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix, netFeesPrefix)
	copy(keyPrefix[3:], b.Bytes())

	byteOrder.PutUint64(scratch, uint64(channel.TotalNetFees))
	return openChanBucket.Put(keyPrefix, scratch)
}

func deleteChanNetFee(openChanBucket *bolt.Bucket, chanID []byte) error {
	keyPrefix := make([]byte, 3+len(chanID))
	copy(keyPrefix, netFeesPrefix)
	copy(keyPrefix[3:], chanID)
	return openChanBucket.Delete(keyPrefix)
}

func fetchChanNetFee(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix, netFeesPrefix)
	copy(keyPrefix[3:], b.Bytes())

	feeBytes := openChanBucket.Get(keyPrefix)
	channel.TotalNetFees = byteOrder.Uint64(feeBytes)

	return nil
}

func putChannelIDs(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	// TODO(roabeef): just pass in chanID everywhere for puts
	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}

	// Construct the id key: cid || channelID.
	// TODO(roasbeef): abstract out to func
	idKey := make([]byte, len(chanIDKey)+b.Len())
	copy(idKey[:3], chanIDKey)
	copy(idKey[3:], b.Bytes())

	return nodeChanBucket.Put(idKey, channel.TheirLNID[:])
}

func deleteChannelIDs(nodeChanBucket *bolt.Bucket, chanID []byte) error {
	idKey := make([]byte, len(chanIDKey)+len(chanID))
	copy(idKey[:3], chanIDKey)
	copy(idKey[3:], chanID)
	return nodeChanBucket.Delete(idKey)
}

func fetchChannelIDs(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}

	// Construct the id key: cid || channelID.
	idKey := make([]byte, len(chanIDKey)+b.Len())
	copy(idKey[:3], chanIDKey)
	copy(idKey[3:], b.Bytes())

	idBytes := nodeChanBucket.Get(idKey)
	copy(channel.TheirLNID[:], idBytes)

	return nil
}

func putChanCommitKeys(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {

	// Construct the key which stores the commitment keys: ckk || channelID.
	// TODO(roasbeef): factor into func
	var bc bytes.Buffer
	if err := writeOutpoint(&bc, channel.ChanID); err != nil {
		return err
	}
	commitKey := make([]byte, len(commitKeys)+bc.Len())
	copy(commitKey[:3], commitKeys)
	copy(commitKey[3:], bc.Bytes())

	var b bytes.Buffer

	if _, err := b.Write(channel.TheirCommitKey.SerializeCompressed()); err != nil {
		return err
	}

	if _, err := b.Write(channel.OurCommitKey.SerializeCompressed()); err != nil {
		return err
	}

	return nodeChanBucket.Put(commitKey, b.Bytes())
}

func deleteChanCommitKeys(nodeChanBucket *bolt.Bucket, chanID []byte) error {
	commitKey := make([]byte, len(commitKeys)+len(chanID))
	copy(commitKey[:3], commitKeys)
	copy(commitKey[3:], chanID)
	return nodeChanBucket.Delete(commitKey)
}

func fetchChanCommitKeys(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {

	// Construct the key which stores the commitment keys: ckk || channelID.
	// TODO(roasbeef): factor into func
	var bc bytes.Buffer
	if err := writeOutpoint(&bc, channel.ChanID); err != nil {
		return err
	}
	commitKey := make([]byte, len(commitKeys)+bc.Len())
	copy(commitKey[:3], commitKeys)
	copy(commitKey[3:], bc.Bytes())

	var err error
	keyBytes := nodeChanBucket.Get(commitKey)

	channel.TheirCommitKey, err = btcec.ParsePubKey(keyBytes[:33], btcec.S256())
	if err != nil {
		return err
	}

	channel.OurCommitKey, err = btcec.ParsePubKey(keyBytes[33:], btcec.S256())
	if err != nil {
		return err
	}

	return nil
}

func putChanCommitTxns(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var bc bytes.Buffer
	if err := writeOutpoint(&bc, channel.ChanID); err != nil {
		return err
	}
	txnsKey := make([]byte, len(commitTxnsKey)+bc.Len())
	copy(txnsKey[:3], commitTxnsKey)
	copy(txnsKey[3:], bc.Bytes())

	var b bytes.Buffer

	if err := channel.OurCommitTx.Serialize(&b); err != nil {
		return err
	}

	if err := wire.WriteVarBytes(&b, 0, channel.OurCommitSig); err != nil {
		return err
	}

	// TODO(roasbeef): should move this into putChanFundingInfo
	scratch := make([]byte, 4)
	byteOrder.PutUint32(scratch, channel.LocalCsvDelay)
	if _, err := b.Write(scratch); err != nil {
		return err
	}
	byteOrder.PutUint32(scratch, channel.RemoteCsvDelay)
	if _, err := b.Write(scratch); err != nil {
		return err
	}

	return nodeChanBucket.Put(txnsKey, b.Bytes())
}

func deleteChanCommitTxns(nodeChanBucket *bolt.Bucket, chanID []byte) error {
	txnsKey := make([]byte, len(commitTxnsKey)+len(chanID))
	copy(txnsKey[:3], commitTxnsKey)
	copy(txnsKey[3:], chanID)
	return nodeChanBucket.Delete(txnsKey)
}

func fetchChanCommitTxns(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var bc bytes.Buffer
	var err error
	if err = writeOutpoint(&bc, channel.ChanID); err != nil {
		return err
	}
	txnsKey := make([]byte, len(commitTxnsKey)+bc.Len())
	copy(txnsKey[:3], commitTxnsKey)
	copy(txnsKey[3:], bc.Bytes())

	txnBytes := bytes.NewReader(nodeChanBucket.Get(txnsKey))

	channel.OurCommitTx = wire.NewMsgTx()
	if err = channel.OurCommitTx.Deserialize(txnBytes); err != nil {
		return err
	}

	channel.OurCommitSig, err = wire.ReadVarBytes(txnBytes, 0, 80, "")
	if err != nil {
		return err
	}

	scratch := make([]byte, 4)

	if _, err := txnBytes.Read(scratch); err != nil {
		return err
	}
	channel.LocalCsvDelay = byteOrder.Uint32(scratch)

	if _, err := txnBytes.Read(scratch); err != nil {
		return err
	}
	channel.RemoteCsvDelay = byteOrder.Uint32(scratch)

	return nil
}

func putChanFundingInfo(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var bc bytes.Buffer
	if err := writeOutpoint(&bc, channel.ChanID); err != nil {
		return err
	}
	fundTxnKey := make([]byte, len(fundingTxnKey)+bc.Len())
	copy(fundTxnKey[:3], fundingTxnKey)
	copy(fundTxnKey[3:], bc.Bytes())

	var b bytes.Buffer

	if err := writeOutpoint(&b, channel.FundingOutpoint); err != nil {
		return err
	}

	ourSerKey := channel.OurMultiSigKey.SerializeCompressed()
	if err := wire.WriteVarBytes(&b, 0, ourSerKey); err != nil {
		return err
	}
	theirSerKey := channel.TheirMultiSigKey.SerializeCompressed()
	if err := wire.WriteVarBytes(&b, 0, theirSerKey); err != nil {
		return err
	}

	if err := wire.WriteVarBytes(&b, 0, channel.FundingRedeemScript[:]); err != nil {
		return err
	}

	scratch := make([]byte, 8)
	byteOrder.PutUint64(scratch, uint64(channel.CreationTime.Unix()))

	if _, err := b.Write(scratch); err != nil {
		return err
	}

	return nodeChanBucket.Put(fundTxnKey, b.Bytes())
}

func deleteChanFundingInfo(nodeChanBucket *bolt.Bucket, chanID []byte) error {
	fundTxnKey := make([]byte, len(fundingTxnKey)+len(chanID))
	copy(fundTxnKey[:3], fundingTxnKey)
	copy(fundTxnKey[3:], chanID)
	return nodeChanBucket.Delete(fundTxnKey)
}

func fetchChanFundingInfo(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}
	fundTxnKey := make([]byte, len(fundingTxnKey)+b.Len())
	copy(fundTxnKey[:3], fundingTxnKey)
	copy(fundTxnKey[3:], b.Bytes())

	infoBytes := bytes.NewReader(nodeChanBucket.Get(fundTxnKey))

	// TODO(roasbeef): can remove as channel ID *is* the funding point now.
	channel.FundingOutpoint = &wire.OutPoint{}
	if err := readOutpoint(infoBytes, channel.FundingOutpoint); err != nil {
		return err
	}

	ourKeyBytes, err := wire.ReadVarBytes(infoBytes, 0, 34, "")
	if err != nil {
		return err
	}
	channel.OurMultiSigKey, err = btcec.ParsePubKey(ourKeyBytes, btcec.S256())
	if err != nil {
		return err
	}

	theirKeyBytes, err := wire.ReadVarBytes(infoBytes, 0, 34, "")
	if err != nil {
		return err
	}
	channel.TheirMultiSigKey, err = btcec.ParsePubKey(theirKeyBytes, btcec.S256())
	if err != nil {
		return err
	}

	channel.FundingRedeemScript, err = wire.ReadVarBytes(infoBytes, 0, 520, "")
	if err != nil {
		return err
	}

	scratch := make([]byte, 8)
	if _, err := infoBytes.Read(scratch); err != nil {
		return err
	}
	unixSecs := byteOrder.Uint64(scratch)
	channel.CreationTime = time.Unix(int64(unixSecs), 0)

	return nil
}

func putChanEklremState(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var bc bytes.Buffer
	if err := writeOutpoint(&bc, channel.ChanID); err != nil {
		return err
	}

	elkremKey := make([]byte, len(elkremStateKey)+bc.Len())
	copy(elkremKey[:3], elkremStateKey)
	copy(elkremKey[3:], bc.Bytes())

	var b bytes.Buffer

	revKey := channel.TheirCurrentRevocation.SerializeCompressed()
	if err := wire.WriteVarBytes(&b, 0, revKey); err != nil {
		return err
	}

	if _, err := b.Write(channel.TheirCurrentRevocationHash[:]); err != nil {
		return err
	}

	// TODO(roasbeef): shouldn't be storing on disk, should re-derive as
	// needed
	senderBytes := channel.LocalElkrem.ToBytes()
	if err := wire.WriteVarBytes(&b, 0, senderBytes); err != nil {
		return err
	}

	reciverBytes, err := channel.RemoteElkrem.ToBytes()
	if err != nil {
		return err
	}
	if err := wire.WriteVarBytes(&b, 0, reciverBytes); err != nil {
		return err
	}

	return nodeChanBucket.Put(elkremKey, b.Bytes())
}

func deleteChanEklremState(nodeChanBucket *bolt.Bucket, chanID []byte) error {
	elkremKey := make([]byte, len(elkremStateKey)+len(chanID))
	copy(elkremKey[:3], elkremStateKey)
	copy(elkremKey[3:], chanID)
	return nodeChanBucket.Delete(elkremKey)
}

func fetchChanEklremState(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}
	elkremKey := make([]byte, len(elkremStateKey)+b.Len())
	copy(elkremKey[:3], elkremStateKey)
	copy(elkremKey[3:], b.Bytes())

	elkremStateBytes := bytes.NewReader(nodeChanBucket.Get(elkremKey))

	revKeyBytes, err := wire.ReadVarBytes(elkremStateBytes, 0, 1000, "")
	if err != nil {
		return err
	}
	channel.TheirCurrentRevocation, err = btcec.ParsePubKey(revKeyBytes, btcec.S256())
	if err != nil {
		return err
	}

	if _, err := elkremStateBytes.Read(channel.TheirCurrentRevocationHash[:]); err != nil {
		return err
	}

	// TODO(roasbeef): should be rederiving on fly, or encrypting on disk.
	senderBytes, err := wire.ReadVarBytes(elkremStateBytes, 0, 1000, "")
	if err != nil {
		return err
	}
	elkremRoot, err := wire.NewShaHash(senderBytes)
	if err != nil {
		return err
	}
	channel.LocalElkrem = elkrem.NewElkremSender(*elkremRoot)

	reciverBytes, err := wire.ReadVarBytes(elkremStateBytes, 0, 1000, "")
	if err != nil {
		return err
	}
	remoteE, err := elkrem.ElkremReceiverFromBytes(reciverBytes)
	if err != nil {
		return err
	}
	channel.RemoteElkrem = remoteE

	return nil
}

func putChanDeliveryScripts(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var bc bytes.Buffer
	if err := writeOutpoint(&bc, channel.ChanID); err != nil {
		return err
	}
	deliveryKey := make([]byte, len(deliveryScriptsKey)+bc.Len())
	copy(deliveryKey[:3], deliveryScriptsKey)
	copy(deliveryKey[3:], bc.Bytes())

	var b bytes.Buffer
	if err := wire.WriteVarBytes(&b, 0, channel.OurDeliveryScript); err != nil {
		return err
	}
	if err := wire.WriteVarBytes(&b, 0, channel.TheirDeliveryScript); err != nil {
		return err
	}

	return nodeChanBucket.Put(deliveryScriptsKey, b.Bytes())
}

func deleteChanDeliveryScripts(nodeChanBucket *bolt.Bucket, chanID []byte) error {
	deliveryKey := make([]byte, len(deliveryScriptsKey)+len(chanID))
	copy(deliveryKey[:3], deliveryScriptsKey)
	copy(deliveryKey[3:], chanID)
	return nodeChanBucket.Delete(deliveryScriptsKey)
}

func fetchChanDeliveryScripts(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}
	deliveryKey := make([]byte, len(deliveryScriptsKey)+b.Len())
	copy(deliveryKey[:3], deliveryScriptsKey)
	copy(deliveryKey[3:], b.Bytes())

	var err error
	deliveryBytes := bytes.NewReader(nodeChanBucket.Get(deliveryScriptsKey))

	channel.OurDeliveryScript, err = wire.ReadVarBytes(deliveryBytes, 0, 520, "")
	if err != nil {
		return err
	}

	channel.TheirDeliveryScript, err = wire.ReadVarBytes(deliveryBytes, 0, 520, "")
	if err != nil {
		return err
	}

	return nil
}

// htlcDiskSize represents the number of btyes a serialized HTLC takes up on
// disk. The size of an HTLC on disk is 49 bytes total: incoming (1) + amt (8)
// + rhash (32) + timeouts (8)
const htlcDiskSize = 1 + 8 + 32 + 4 + 4

func serializeHTLC(w io.Writer, h *HTLC) error {
	var buf [htlcDiskSize]byte

	var boolByte [1]byte
	if h.Incoming {
		boolByte[0] = 1
	} else {
		boolByte[0] = 0
	}

	var n int
	n += copy(buf[:], boolByte[:])
	byteOrder.PutUint64(buf[n:], uint64(h.Amt))
	n += 8
	n += copy(buf[n:], h.RHash[:])
	byteOrder.PutUint32(buf[n:], h.RefundTimeout)
	n += 4
	byteOrder.PutUint32(buf[n:], h.RevocationTimeout)
	n += 4

	if _, err := w.Write(buf[:]); err != nil {
		return err
	}

	return nil
}

func deserializeHTLC(r io.Reader) (*HTLC, error) {
	h := &HTLC{}

	var scratch [8]byte

	if _, err := r.Read(scratch[:1]); err != nil {
		return nil, err
	}
	if scratch[0] == 1 {
		h.Incoming = true
	} else {
		h.Incoming = false
	}

	if _, err := r.Read(scratch[:]); err != nil {
		return nil, err
	}
	h.Amt = btcutil.Amount(byteOrder.Uint64(scratch[:]))

	if _, err := r.Read(h.RHash[:]); err != nil {
		return nil, err
	}

	if _, err := r.Read(scratch[:4]); err != nil {
		return nil, err
	}
	h.RefundTimeout = byteOrder.Uint32(scratch[:4])

	if _, err := r.Read(scratch[:4]); err != nil {
		return nil, err
	}
	h.RevocationTimeout = byteOrder.Uint32(scratch[:])

	return h, nil
}

func serializeChannelDelta(w io.Writer, delta *ChannelDelta) error {
	// TODO(roasbeef): could use compression here to reduce on-disk space.
	var scratch [8]byte
	byteOrder.PutUint64(scratch[:], uint64(delta.LocalBalance))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}
	byteOrder.PutUint64(scratch[:], uint64(delta.RemoteBalance))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	byteOrder.PutUint32(scratch[:4], delta.UpdateNum)
	if _, err := w.Write(scratch[:4]); err != nil {
		return err
	}

	numHtlcs := uint64(len(delta.Htlcs))
	if err := wire.WriteVarInt(w, 0, numHtlcs); err != nil {
		return err
	}
	for _, htlc := range delta.Htlcs {
		if err := serializeHTLC(w, htlc); err != nil {
			return err
		}
	}

	return nil
}

func deserializeChannelDelta(r io.Reader) (*ChannelDelta, error) {
	var (
		err     error
		scratch [8]byte
	)

	delta := &ChannelDelta{}

	if _, err := r.Read(scratch[:]); err != nil {
		return nil, err
	}
	delta.LocalBalance = btcutil.Amount(byteOrder.Uint64(scratch[:]))
	if _, err := r.Read(scratch[:]); err != nil {
		return nil, err
	}
	delta.RemoteBalance = btcutil.Amount(byteOrder.Uint64(scratch[:]))

	if _, err := r.Read(scratch[:4]); err != nil {
		return nil, err
	}
	delta.UpdateNum = byteOrder.Uint32(scratch[:4])

	numHtlcs, err := wire.ReadVarInt(r, 0)
	if err != nil {
		return nil, err
	}
	delta.Htlcs = make([]*HTLC, numHtlcs)
	for i := uint64(0); i < numHtlcs; i++ {
		htlc, err := deserializeHTLC(r)
		if err != nil {
			return nil, err
		}

		delta.Htlcs[i] = htlc
	}

	return delta, nil
}

func appendChannelLogEntry(log *bolt.Bucket, delta *ChannelDelta,
	chanPoint *wire.OutPoint) error {

	// First construct the key for this particular log entry.  The key for
	// each newly added log entry is: channelPoint || stateNum.
	var logEntrykey [36 + 4]byte
	copy(logEntrykey[:], chanPoint.Hash[:])
	var scratch [4]byte
	byteOrder.PutUint32(scratch[:], delta.UpdateNum)
	copy(logEntrykey[36:], scratch[:])

	// With the key constructed, serialize the delta to raw bytes, then
	// write the new state to disk.
	var b bytes.Buffer
	if err := serializeChannelDelta(&b, delta); err != nil {
		return err
	}

	return log.Put(logEntrykey[:], b.Bytes())
}

func fetchChannelLogEntry(log *bolt.Bucket, chanPoint *wire.OutPoint,
	updateNum uint32) (*ChannelDelta, error) {

	// First construct the key for this particular log entry.  The key for
	// each newly added log entry is: channelPoint || stateNum.
	// TODO(roasbeef): make into func..
	var logEntrykey [36 + 4]byte
	copy(logEntrykey[:], chanPoint.Hash[:])
	var scratch [4]byte
	byteOrder.PutUint32(scratch[:], updateNum)
	copy(logEntrykey[36:], scratch[:])

	deltaBytes := log.Get(logEntrykey[:])
	if deltaBytes == nil {
		return nil, fmt.Errorf("log entry not found")
	}

	deltaReader := bytes.NewReader(deltaBytes)

	return deserializeChannelDelta(deltaReader)
}

func writeOutpoint(w io.Writer, o *wire.OutPoint) error {
	// TODO(roasbeef): make all scratch buffers on the stack
	scratch := make([]byte, 4)

	// TODO(roasbeef): write raw 32 bytes instead of wasting the extra
	// byte.
	if err := wire.WriteVarBytes(w, 0, o.Hash[:]); err != nil {
		return err
	}

	byteOrder.PutUint32(scratch, o.Index)
	if _, err := w.Write(scratch); err != nil {
		return err
	}

	return nil
}

func readOutpoint(r io.Reader, o *wire.OutPoint) error {
	scratch := make([]byte, 4)

	txid, err := wire.ReadVarBytes(r, 0, 32, "prevout")
	if err != nil {
		return err
	}
	copy(o.Hash[:], txid)

	if _, err := r.Read(scratch); err != nil {
		return err
	}
	o.Index = byteOrder.Uint32(scratch)

	return nil
}
