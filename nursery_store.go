package main

import (
	"bytes"
	"errors"

	"github.com/boltdb/bolt"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
)

// NurseryStore abstracts the persistent storage layer for the utxo nursery.
// Concretely, it stores commitment and htlc outputs that until any time-bounded
// constraints have fully matured. The store exposes methods for enumerating
// its contents, and persisting state transitions detected by the utxo nursery.
type NurseryStore interface {

	// Incubation Entry Points.

	// EnterCrib accepts a new htlc output that the nursery will incubate
	// through its two-stage process of sweeping funds back to the user's
	// wallet. These outputs are persisted in the nursery store's crib
	// bucket, and will be revisited after the output's CLTV has expired.
	EnterCrib(*babyOutput) error

	// EnterPreschool accepts a new commitment output that the nursery will
	// incubate through a single stage before sweeping. Outputs are stored
	// in the preschool bucket until the commitment transaction has been
	// confirmed, at which point they will be moved to the kindergarten
	// bucket.
	EnterPreschool(*kidOutput) error

	// On-chain Driven State Transtitions.

	// CribToKinder atomically moves a babyOutput in the crib bucket to the
	// kindergarten bucket. The now mature kidOutput contained in the
	// babyOutput will be stored as it waits out the kidOutput's CSV delay.
	CribToKinder(*babyOutput) error

	// PreschoolToKinder atomically moves a kidOutput from the preschool
	// bucket to the kindergarten bucket. This transition should be executed
	// after receiving confirmation of the preschool output's commitment
	// transaction.
	PreschoolToKinder(*kidOutput) error

	// AwardDiplomas accepts a variadic number of kidOutputs from the
	// kindergarten bucket, and removes their corresponding entries from the
	// height and channel indexes.  If this method detects that all outputs
	// for a particular contract have been incubated, it returns the channel
	// points that are ready to be marked as fully closed.
	// TODO: make this handle one output at a time?
	AwardDiplomas(...kidOutput) ([]wire.OutPoint, error)

	// FinalizeClass accepts a block height as a parameter and purges its
	// persistent state for all outputs at that height. During a restart,
	// the utxo nursery will begin it's recovery procedure from the next
	// height that has yet to be finalized. This block height should lag
	// beyond the best height for this chain as a measure of reorg
	// protection.
	FinalizeClass(height uint32) error

	// State Bucket Enumeration.

	// FetchCribs returns a list of babyOutputs in the crib bucket whose
	// CLTV delay expires at the provided block height.
	FetchCribs(height uint32) ([]babyOutput, error)

	// FetchKindergartens returns a list of kidOutputs in the kindergarten
	// bucket whose CSV delay expires at the provided block height.
	FetchKindergartens(height uint32) ([]kidOutput, error)

	// FetchPreschools returns a list of all outputs currently stored in the
	// preschool bucket.
	FetchPreschools() ([]kidOutput, error)

	// Channel Output Enumeration.

	// ForChanOutputs iterates over all outputs being incubated for a
	// particular channel point. This method accepts a callback that allows
	// the caller to process each key-value pair. The key will be a prefixed
	// outpoint, and the value will be the serialized bytes for an output,
	// whose type should be inferred from the key's prefix.
	ForChanOutputs(*wire.OutPoint, func([]byte, []byte) error) error

	// The Point of No Return.

	// LastFinalizedHeight returns the last block height for which the
	// nursery store has purged all persistent state.
	LastFinalizedHeight() (uint32, error)
}

// prefixChainKey creates the root level keys for the nursery store. The keys
// are comprised of a nursery-specific prefix and the intended chain hash that
// this nursery store will be used for. This allows multiple nursery stores to
// isolate their state when operating on multiple chains or forks.
func prefixChainKey(sysPrefix []byte, hash *chainhash.Hash) ([]byte, error) {
	// Create a buffer to which we will write the system prefix, e.g.
	// "utxn", followed by the provided chain hash.
	var pfxChainBuffer bytes.Buffer
	if _, err := pfxChainBuffer.Write(sysPrefix); err != nil {
		return nil, err
	}

	if _, err := pfxChainBuffer.Write(hash[:]); err != nil {
		return nil, err
	}

	return pfxChainBuffer.Bytes(), nil
}

// prefixOutputKey creates a serialized key that prefixes the serialized
// outpoint with the provided state prefix. The returned bytes will be of the
// form <prefix><outpoint>.
func prefixOutputKey(statePrefix []byte,
	outpoint *wire.OutPoint) ([]byte, error) {

	// Create a buffer to which we will first write the state prefix,
	// followed by the outpoint.
	var pfxOutputBuffer bytes.Buffer
	if _, err := pfxOutputBuffer.Write(statePrefix); err != nil {
		return nil, err
	}

	err := writeOutpoint(&pfxOutputBuffer, outpoint)
	if err != nil {
		return nil, err
	}

	return pfxOutputBuffer.Bytes(), nil
}

var (
	// utxnChainPrefix is used to prefix a particular chain hash and create
	// the root-level, chain-segmented bucket for each nursery store.
	utxnChainPrefix = []byte("utxn")

	// lastFinalizedHeightKey is a static key used to locate nursery store's
	// last finalized height.
	lastFinalizedHeightKey = []byte("last-finalized-height")

	// channelIndexKey is a static key used to lookup the bucket containing
	// all of the nursery's active channels.
	channelIndexKey = []byte("channel-index")

	// channelIndexKey is a static key used to retrieve a directory
	// containing all heights for which the nursery will need to take
	// action.
	heightIndexKey = []byte("height-index")

	// cribPrefix is the state prefix given to htlc outputs waiting for
	// their first-stage, absolute locktime to elapse.
	cribPrefix = []byte("crib")

	// psclPrefix is the state prefix given to commitment outputs awaiting
	// the // confirmation of the commitment transaction, as this solidifies
	// the absolute height at which they can be spent.
	psclPrefix = []byte("pscl")

	// kndrPrefix is the state prefix given to all CSV delayed outputs,
	// either from the commitment transaction, or a stage-one htlc
	// transaction, whose maturity height has solidified. Outputs marked in
	// this state are in their final stage of incubation withn the nursery,
	// and will be swept into the wallet after waiting out the relative
	// timelock.
	kndrPrefix = []byte("kndr")
)

//	              Overview of Nursery Store Storage Hierarchy
//
//   CHAIN SEGMENTATION
//
//   The root directory of a nursery store is bucketed by the chain hash and
//   the 'utxn' prefix. This allows multiple utxo nurseries for distinct chains
//   to simultaneously use the same channel.DB instance. This is critical for
//   providing replay protection and more to isolate chain-specific data in the
//   multichain setting.
//
//   utxn<chain-hash>/
//   |
//   |   LAST FINALIZED HEIGHT
//   |
//   |   Each nursery store tracks a "last finalized height", which records the
//   |   most recent block height for which the nursery store has purged all
//   |   state. This value lags behind the best block height for reorg safety,
//   |   and serves as a starting height for rescans after a restart.
//   |
//   ├── last-finalized-height-key: <last-finalized-height>
//   |
//   |   CHANNEL INDEX
//   |
//   |   The channel index contains a directory for each channel that has a
//   |   non-zero number of outputs being tracked by the nursery store.
//   |   Inside each channel directory are files contains serialized spendable
//   |   outputs that are awaiting some state transition. The name of each file
//   |   contains the outpoint of the spendable output in the file, and is
//   |   prefixed with 4-byte state prefix, indicating whether the spendable
//   |   output is a crib, preschool, or kindergarten output. The nursery store
//   |   supports the ability to enumerate all outputs for a particular channel,
//   |   which is useful in constructing nursery reports.
//   |
//   ├── channel-index-key/
//   │   ├── <chain-point-1>/                      <- CHANNEL BUCKET
//   |   |   ├── <state-prefix><outpoint-1>: <spendable-output-1>
//   |   |   └── <state-prefix><outpoint-2>: <spendable-output-2>
//   │   ├── <chain-point-2>/
//   |   |   └── <state-prefix><outpoint-3>: <spendable-output-3>
//   │   └── <chain-point-3>/
//   |       ├── <state-prefix><outpoint-4>: <spendable-output-4>
//   |       └── <state-prefix><outpoint-5>: <spendable-output-5>
//   |
//   |   HEIGHT INDEX
//   |
//   |   The height index contains a directory for each height at which the
//   |   nursery still has uncompleted actions. If an output is a crib or
//   |   kindergarten output,
//   |   it will have an associated entry in the height index. Inside a
//   |   particular height directory, the structure is similar to that of the
//   |   channel index, containing multiple channel directories, each of which
//   |   contains subdirectories named with a prefixed outpoint belonging to
//   |   the channel. Enumerating these combinations yields a relative file
//   |   path:
//   |     e.g. <chan-point-3>/<prefix><outpoint-2>/
//   |   that can be queried in the channel index to retrieve the serialized
//   |   output.
//   |
//   └── height-index-key/
//       ├── <height-1>/                             <- HEIGHT BUCKET
//       |   └── <chan-point-3>/                     <- HEIGHT-CHANNEL BUCKET
//       |   |    ├── <state-prefix><outpoint-4>/    <- PREFIXED OUTPOINT
//       |   |    └── <state-prefix><outpoint-5>/
//       |   └── <chan-point-2>/
//       |        └── <state-prefix><outpoint-3>/
//       └── <height-2>/
//           └── <chan-point-1>/
//                └── <state-prefix><outpoint-1>/
//                └── <state-prefix><outpoint-2>/

// nurseryStore is a concrete instantiation of a NurseryStore that is backed by
// a channeldb.DB instance.
type nurseryStore struct {
	chainHash   chainhash.Hash
	pfxChainKey []byte

	db *channeldb.DB
}

// newNurseryStore accepts a chain hash and a channeldb.DB instance, returning
// an instance of nurseryStore who's database is properly segmented for the
// given chain.
func newNurseryStore(chainHash *chainhash.Hash,
	db *channeldb.DB) (*nurseryStore, error) {

	// Prefix the provided chain hash with "utxn" to create the key for the
	// nursery store's root bucket, ensuring each one has proper chain
	// segmentation.
	pfxChainKey, err := prefixChainKey(utxnChainPrefix, chainHash)
	if err != nil {
		return nil, err
	}

	return &nurseryStore{
		chainHash:   *chainHash,
		pfxChainKey: pfxChainKey,
		db:          db,
	}, nil
}

// Incubation Entry Points.

// EnterCrib accepts a new htlc output that the nursery will incubate through
// its two-stage process of sweeping funds back to the user's wallet. These
// outputs are persisted in the nursery store's crib bucket, and will be
// revisited after the output's CLTV has expired.
func (ns *nurseryStore) EnterCrib(bby *babyOutput) error {
	return ns.db.Update(func(tx *bolt.Tx) error {

		// First, retrieve or create the channel bucket corresponding to
		// the baby output's origin channel point.
		chanPoint := bby.OriginChanPoint()
		chanBucket, err := ns.createChannelBucket(tx, chanPoint)
		if err != nil {
			return err
		}

		// Next, retrieve or create the height-channel bucket located in
		// the height bucket corresponding to the baby output's CLTV
		// expiry height.
		hghtChanBucket, err := ns.createHeightChanBucket(tx,
			bby.expiry, chanPoint)
		if err != nil {
			return err
		}

		// Since we are inserting this output into the crib bucket, we
		// create a key that prefixes the baby output's outpoint with
		// the crib prefix.
		pfxOutputKey, err := prefixOutputKey(cribPrefix, bby.OutPoint())
		if err != nil {
			return err
		}

		// Serialize the baby output so that it can be written to the
		// underlying key-value store.
		var babyBuffer bytes.Buffer
		if err := bby.Encode(&babyBuffer); err != nil {
			return err
		}
		babyBytes := babyBuffer.Bytes()

		// Now, insert the serialized output into its channel bucket
		// under the prefixed key created above.
		if err := chanBucket.Put(pfxOutputKey, babyBytes); err != nil {
			return err
		}

		// Finally, create a corresponding bucket in the height-channel
		// bucket for this crib output. The existence of this bucket
		// indicates that the serialized output can be retrieved from
		// the channel bucket using the same prefix key.
		_, err = hghtChanBucket.CreateBucketIfNotExists(pfxOutputKey)

		return err
	})
}

// EnterPreschool accepts a new commitment output that the nursery will
// incubate through a single stage before sweeping. Outputs are stored in the
// preschool bucket until the commitment transaction has been confirmed, at
// which point they will be moved to the kindergarten bucket.
func (ns *nurseryStore) EnterPreschool(kid *kidOutput) error {
	return ns.db.Update(func(tx *bolt.Tx) error {

		// First, retrieve or create the channel bucket corresponding to
		// the baby output's origin channel point.
		chanPoint := kid.OriginChanPoint()
		chanBucket, err := ns.createChannelBucket(tx, chanPoint)
		if err != nil {
			return err
		}

		// Since the babyOutput is being inserted into the preschool
		// bucket, we create a key that prefixes its outpoint with the
		// preschool prefix.
		pfxOutputKey, err := prefixOutputKey(psclPrefix, kid.OutPoint())
		if err != nil {
			return err
		}

		// Serialize the kidOutput and insert it into the channel
		// bucket.
		var kidBuffer bytes.Buffer
		if err := kid.Encode(&kidBuffer); err != nil {
			return err
		}

		return chanBucket.Put(pfxOutputKey, kidBuffer.Bytes())
	})
}

// On-chain Drive State Transitions.

// CribToKinder atomically moves a babyOutput in the crib bucket to the
// kindergarten bucket. The now mature kidOutput contained in the babyOutput
// will be stored as it waits out the kidOutput's CSV delay.
func (ns *nurseryStore) CribToKinder(bby *babyOutput) error {
	return ns.db.Update(func(tx *bolt.Tx) error {

		// First, retrieve or create the channel bucket corresponding to
		// the baby output's origin channel point.
		chanPoint := bby.OriginChanPoint()
		chanBucket, err := ns.createChannelBucket(tx, chanPoint)
		if err != nil {
			return err
		}

		// The babyOutput should currently be stored in the crib bucket.
		// So, we create a key that prefixes the babyOutput's outpoint
		// with the crib prefix, allowing us to reference it in the
		// store.
		pfxOutputKey, err := prefixOutputKey(cribPrefix, bby.OutPoint())
		if err != nil {
			return err
		}

		// Since the babyOutput is being moved to the kindergarten
		// bucket, we remove the entry from the channel bucket under the
		// crib-prefixed outpoint key.
		if err := chanBucket.Delete(pfxOutputKey); err != nil {
			return err
		}

		// Next, retrieve the height-channel bucket located in the
		// height bucket corresponding to the baby output's CLTV expiry
		// height. This bucket should always exist, but if it doesn't
		// then we have nothing to clean up.
		hghtChanBucketCltv := ns.getHeightChanBucket(tx, bby.expiry,
			chanPoint)
		if hghtChanBucketCltv != nil {
			// We successfully located  an existing height chan
			// bucket at this babyOutput's expiry height, proceed by
			// removing it from the index.
			err := hghtChanBucketCltv.DeleteBucket(pfxOutputKey)
			if err != nil {
				return err
			}
		}

		// Since we are moving this output from the crib bucket to the
		// kindergarten bucket, we overwrite the existing prefix of this
		// key with the kindergarten prefix.
		copy(pfxOutputKey, kndrPrefix)

		// Now, serialize babyOutput's encapsulated kidOutput such that
		// it can be written to the channel bucket under the new
		// kindergarten-prefixed key.
		var kidBuffer bytes.Buffer
		if err := bby.kidOutput.Encode(&kidBuffer); err != nil {
			return err
		}
		kidBytes := kidBuffer.Bytes()

		// Persist the serialized kidOutput under the
		// kindergarten-prefixed outpoint key.
		if err := chanBucket.Put(pfxOutputKey, kidBytes); err != nil {
			return err
		}

		// Now, compute the height at which this kidOutput's CSV delay
		// will expire.  This is done by adding the required delay to
		// the block height at which the output was confirmed.
		maturityHeight := bby.ConfHeight() + bby.BlocksToMaturity()

		// Retrive or create a height-channel bucket corresponding to
		// the kidOutput's maturity height.
		hghtChanBucketCsv, err := ns.createHeightChanBucket(tx,
			maturityHeight, chanPoint)
		if err != nil {
			return err
		}

		// Register the kindergarten output's prefixed output key in the
		// height-channel bucket corresponding to its maturity height.
		// This informs the utxo nursery that it should attempt to spend
		// this output when the blockchain reaches the maturity height.
		_, err = hghtChanBucketCsv.CreateBucketIfNotExists(pfxOutputKey)
		if err != nil {
			return err
		}

		// Finally, since we removed a crib output from the height
		// index, we opportunistically prune the height bucket
		// corresponding to the babyOutput's CLTV delay. This allows us
		// to clean up any persistent state as outputs are progressed
		// through the incubation process.
		err = ns.pruneHeight(tx, bby.expiry)
		switch err {
		case nil, ErrBucketDoesNotExist:
			return nil
		case ErrBucketNotEmpty:
			return nil
		default:
			return err
		}
	})
}

// PreschoolToKinder atomically moves a kidOutput from the preschool bucket to
// the kindergarten bucket. This transition should be executed after receiving
// confirmation of the preschool output's commitment transaction.
func (ns *nurseryStore) PreschoolToKinder(kid *kidOutput) error {
	return ns.db.Update(func(tx *bolt.Tx) error {

		// Create or retrieve the channel bucket corresponding to the
		// kid output's origin channel point.
		chanPoint := kid.OriginChanPoint()
		chanBucket, err := ns.createChannelBucket(tx, chanPoint)
		if err != nil {
			return err
		}

		// First, we will attempt to remove the existing serialized
		// output from the channel bucket, where the kid's outpoint will
		// be prefixed by a preschool prefix.

		// Generate the key of existing serialized kid output by
		// prefixing its outpoint with the preschool prefix...
		pfxOutputKey, err := prefixOutputKey(psclPrefix, kid.OutPoint())
		if err != nil {
			return err
		}

		// And remove the old serialized output from the database.
		if err := chanBucket.Delete(pfxOutputKey); err != nil {
			return err
		}

		// Next, we will write the provided kid outpoint to the channel
		// bucket, using a key prefixed by the kindergarten prefix.

		// Convert the preschool prefix key into a kindergarten key for
		// the same outpoint.
		copy(pfxOutputKey, kndrPrefix)

		// Reserialize the kid here to capture any differences in the
		// new and old kid output, such as the confirmation height.
		var kidBuffer bytes.Buffer
		if err := kid.Encode(&kidBuffer); err != nil {
			return err
		}
		kidBytes := kidBuffer.Bytes()

		// And store the kid output in its channel bucket using the
		// kindergarten prefixed key.
		if err := chanBucket.Put(pfxOutputKey, kidBytes); err != nil {
			return err
		}

		// Since the CSV delay on the kid output has now begun ticking,
		// we must insert a record of in the height index to remind us
		// to revisit this output once it has fully matured.

		// Compute the maturity height, by adding the output's CSV delay
		// to its confirmation height.
		maturityHeight := kid.ConfHeight() + kid.BlocksToMaturity()

		// Create or retrieve the height-channel bucket for this
		// channel. This method will first create a height bucket for
		// the given maturity height if none exists.
		hghtChanBucket, err := ns.createHeightChanBucket(tx,
			maturityHeight, chanPoint)
		if err != nil {
			return err
		}

		// Finally, we touch a bucket in the height-channel created
		// above.  The bucket is named using a kindergarten prefixed
		// key, signaling that this CSV delayed output will be ready to
		// broadcast at the maturity height, after a brief period of
		// incubation.
		_, err = hghtChanBucket.CreateBucketIfNotExists(pfxOutputKey)

		return err
	})
}

// AwardDiplomas accepts a list of kidOutputs in the kindergarten bucket,
// removing their corresponding entries from the height and channel indexes.
// If this method detects that all outputs for a particular contract have been
// incubated, it returns the channel points that are ready to be marked as
// fully closed. This method will iterate through the provided kidOutputs and do
// the following:
// 1) Prune the kid height bucket at the kid's confirmation height, if it is
//     empty.
// 2) Prune the channel bucket belonging to the kid's origin channel point, if
//     it is empty.
func (ns *nurseryStore) AwardDiplomas(
	kids ...kidOutput) ([]wire.OutPoint, error) {

	// As we iterate over the kids, we will build a list of the channels
	// which have been pruned entirely from the nursery store. We will
	// return this list to the caller, the utxo nursery, so that it can
	// proceed to mark the channels as closed.
	// TODO(conner): write list of closed channels to separate bucket so
	// that they can be replayed on restart?
	var closedChannelSet = make(map[wire.OutPoint]struct{})
	if err := ns.db.Update(func(tx *bolt.Tx) error {
		for _, kid := range kids {
			// Attempt to prune the height bucket matching the kid
			// output's confirmation height if it contains no active
			// outputs.
			err := ns.pruneHeight(tx, kid.ConfHeight())
			switch err {
			case ErrBucketNotEmpty:
				// Bucket still has active outputs, proceed to
				// prune channel bucket.

			case ErrBucketDoesNotExist:
				// Bucket was previously pruned by another
				// graduating output.

			case nil:
				// Bucket was pruned successfully and no errors
				// were encounter.
				utxnLog.Infof("Height bucket %d pruned",
					kid.ConfHeight())

			default:
				// Unexpected database error.
				return err
			}

			outpoint := kid.OutPoint()
			chanPoint := kid.OriginChanPoint()

			// Remove the outpoint belonging to the kid output from
			// it's channel bucket, then attempt to prune the
			// channel bucket if it is now empty.
			err = ns.deleteAndPruneChannel(tx, chanPoint, outpoint)
			switch err {
			case ErrBucketNotEmpty:
				// Bucket still has active outputs, continue to
				// next kid to avoid adding this channel point
				// to the set of channels to be closed.
				continue

			case ErrBucketDoesNotExist:
				// Bucket may have been removed previously,
				// allow this to fall through and ensure the
				// channel point is added to the set channels to
				// be closed.

			case nil:
				// Channel bucket was successfully pruned,
				// proceed to add to set of channels to be
				// closed.
				utxnLog.Infof("Height bucket %d pruned",
					kid.ConfHeight())

			default:
				// Uh oh, database error.
				return err
			}

			// If we've arrived here, we have encountered no
			// database errors and a bucket was either successfully
			// pruned or already has been. Thus it is safe to add it
			// to our set of closed channels to be closed, since
			// these may need to be replayed to ensure the channel
			// database is aware that incubation has completed.
			closedChannelSet[*chanPoint] = struct{}{}
		}

		return nil
	}); err != nil {
		return nil, err
	}

	// Convert our set of channels to be closed into a list.
	channelsToBeClosed := make([]wire.OutPoint, 0, len(closedChannelSet))
	for chanPoint := range closedChannelSet {
		channelsToBeClosed = append(channelsToBeClosed, chanPoint)
	}

	utxnLog.Infof("Channels to be marked fully closed: %x",
		channelsToBeClosed)

	return channelsToBeClosed, nil
}

// FinalizeClass accepts a block height as a parameter and purges its
// persistent state for all outputs at that height. During a restart, the utxo
// nursery will begin it's recovery procedure from the next height that has
// yet to be finalized.
func (ns *nurseryStore) FinalizeClass(height uint32) error {
	utxnLog.Infof("Finalizing class at height %v", height)
	return ns.db.Update(func(tx *bolt.Tx) error {
		return ns.putLastFinalizedHeight(tx, height)
	})
}

// State Bucket Enumeration.

// FetchCribs returns a list of babyOutputs in the crib bucket whose CLTV
// delay expires at the provided block height.
func (ns *nurseryStore) FetchCribs(height uint32) ([]babyOutput, error) {
	// Construct a list of all babyOutputs that need TLC at the provided
	// block height.
	var babies []babyOutput
	if err := ns.forEachHeightPrefix(cribPrefix, height,
		func(buf []byte) error {

			// We will attempt to deserialize all outputs stored
			// with the crib prefix into babyOutputs, since this is
			// the expected type that would have been serialized
			// previously.
			var bby babyOutput
			if err := bby.Decode(bytes.NewReader(buf)); err != nil {
				return err
			}

			// Append the deserialized object to our list of
			// babyOutputs.
			babies = append(babies, bby)

			return nil

		}); err != nil {
		return nil, err
	}

	return babies, nil
}

// FetchKindergartens returns a list of kidOutputs in the kindergarten bucket
// whose CSV delay expires at the provided block height.
func (ns *nurseryStore) FetchKindergartens(height uint32) ([]kidOutput, error) {
	// Construct a list of all kidOutputs that mature at the provided block
	// height.
	var kids []kidOutput
	if err := ns.forEachHeightPrefix(kndrPrefix, height,
		func(buf []byte) error {

			// We will attempt to deserialize all outputs stored
			// with the kindergarten prefix into kidOutputs, since
			// this is the expected type that would have been
			// serialized previously.
			var kid kidOutput
			if err := kid.Decode(bytes.NewReader(buf)); err != nil {
				return err
			}

			// Append the deserialized object to our list of
			// kidOutputs.
			kids = append(kids, kid)

			return nil

		}); err != nil {
		return nil, err
	}

	return kids, nil
}

// FetchPreschools returns a list of all outputs currently stored in the
// preschool bucket.
func (ns *nurseryStore) FetchPreschools() ([]kidOutput, error) {
	var kids []kidOutput
	if err := ns.db.View(func(tx *bolt.Tx) error {

		// Retrieve the existing chain bucket for this nursery store.
		chainBucket := tx.Bucket(ns.pfxChainKey)
		if chainBucket == nil {
			return nil
		}

		// Load the existing channel index from the chain bucket.
		chanIndex := chainBucket.Bucket(channelIndexKey)
		if chanIndex == nil {
			return nil
		}

		// Construct a list of all channels in the channel index that
		// are currently being tracked by the nursery store.
		var activeChannels [][]byte
		if err := chanIndex.ForEach(func(chanBytes, _ []byte) error {
			activeChannels = append(activeChannels, chanBytes)
			return nil
		}); err != nil {
			return err
		}

		// Iterate over all of the accumulated channels, and do a prefix
		// scan inside of each channel bucket. Each output found that
		// has a preschool prefix will be deserialized into a kidOutput,
		// and added to our list of preschool outputs to return to the
		// caller.
		for _, chanBytes := range activeChannels {
			// Retrieve the channel bucket associated with this
			// channel.
			chanBucket := chanIndex.Bucket(chanBytes)
			if chanBucket == nil {
				continue
			}

			// All of the outputs of interest will start with the
			// "pscl" prefix. So, we will perform a prefix scan of
			// the channel bucket to efficiently enumerate all the
			// desired outputs.
			c := chanBucket.Cursor()

			// Seek and iterate over all outputs starting with the
			// prefix "pscl".
			pfxOutputKey, kidBytes := c.Seek(psclPrefix)
			for bytes.HasPrefix(pfxOutputKey, psclPrefix) {

				// Deserialize each output as a kidOutput, since
				// this should have been the type that was
				// serialized when it was written to disk.
				var psclOutput kidOutput
				psclReader := bytes.NewReader(kidBytes)
				err := psclOutput.Decode(psclReader)
				if err != nil {
					return err
				}

				// Add the deserialized output to our list of
				// preschool outputs.
				kids = append(kids, psclOutput)

				// Advance to the subsequent key-value pair of
				// the prefix scan.
				pfxOutputKey, kidBytes = c.Next()
			}
		}

		return nil
	}); err != nil {
		return nil, err
	}

	return kids, nil
}

// Channel Output Enumberation.

// ForChanOutputs iterates over all outputs being incubated for a particular
// channel point. This method accepts a callback that allows the caller to
// process each key-value pair. The key will be a prefixed outpoint, and the
// value will be the serialized bytes for an output, whose type should be
// inferred from the key's prefix.
// NOTE: The callback should be not modify the provided byte slices and is
// preferably non-blocking.
func (ns *nurseryStore) ForChanOutputs(chanPoint *wire.OutPoint,
	callback func([]byte, []byte) error) error {

	return ns.db.View(func(tx *bolt.Tx) error {
		chanBucket := ns.getChannelBucket(tx, chanPoint)
		if chanBucket == nil {
			return ErrContractNotFound
		}

		return chanBucket.ForEach(callback)
	})
}

// The Point of No Return.

// LastFinalizedHeight returns the last block height for which the nursery
// store has purged all persistent state. This occurs after a fixed interval
// for reorg safety.
func (ns *nurseryStore) LastFinalizedHeight() (uint32, error) {
	var lastFinalizedHeight uint32
	err := ns.db.View(func(tx *bolt.Tx) error {
		lastHeight, err := ns.getLastFinalizedHeight(tx)
		if err != nil {
			return err
		}

		lastFinalizedHeight = lastHeight

		return nil
	})
	return lastFinalizedHeight, err
}

// getLastFinalizedHeight is a helper method that retrieves the last height for
// which the database finalized its persistent state.
func (ns *nurseryStore) getLastFinalizedHeight(tx *bolt.Tx) (uint32, error) {
	// Retrieve the chain bucket associated with the given nursery store.
	chainBucket := tx.Bucket(ns.pfxChainKey)
	if chainBucket == nil {
		return 0, nil
	}

	// Lookup the last finalized height in the top-level chain bucket.
	heightBytes := chainBucket.Get(lastFinalizedHeightKey)

	// If the resulting bytes are not sized like a uint32, then we have
	// never finalized, so we return 0.
	if len(heightBytes) != 4 {
		return 0, nil
	}

	// Otherwise, parse the bytes and return the last finalized height.
	return byteOrder.Uint32(heightBytes), nil
}

// pubLastFinalizedHeight is a helper method that writes the provided height
// under the last finalized height key.
func (ns *nurseryStore) putLastFinalizedHeight(tx *bolt.Tx,
	height uint32) error {

	// Ensure that the chain bucket for this nursery store exists.
	chainBucket, err := tx.CreateBucketIfNotExists(ns.pfxChainKey)
	if err != nil {
		return err
	}

	// Serialize the provided last-finalized height, and store it in the
	// top-level chain bucket for this nursery store.
	var lastHeightBytes [4]byte
	byteOrder.PutUint32(lastHeightBytes[:], height)

	return chainBucket.Put(lastFinalizedHeightKey, lastHeightBytes[:])
}

// createChannelBucket creates or retrieves a channel bucket for the provided
// channel point.
func (ns *nurseryStore) createChannelBucket(tx *bolt.Tx,
	chanPoint *wire.OutPoint) (*bolt.Bucket, error) {

	// Ensure that the chain bucket for this nursery store exists.
	chainBucket, err := tx.CreateBucketIfNotExists(ns.pfxChainKey)
	if err != nil {
		return nil, err
	}

	// Ensure that the channel index has been properly initialized for this
	// chain.
	chanIndex, err := chainBucket.CreateBucketIfNotExists(channelIndexKey)
	if err != nil {
		return nil, err
	}

	// Serialize the provided channel point, as this provides the name of
	// the channel bucket of interest.
	var chanBuffer bytes.Buffer
	if err := writeOutpoint(&chanBuffer, chanPoint); err != nil {
		return nil, err
	}

	// Finally, create or retrieve the channel bucket using the serialized
	// key.
	return chanIndex.CreateBucketIfNotExists(chanBuffer.Bytes())
}

// getChannelBucket retrieves an existing channel bucket from the nursery store,
// using the given channel point.  If the bucket does not exist, or any bucket
// along its path does not exist, a nil value is returned.
func (ns *nurseryStore) getChannelBucket(tx *bolt.Tx,
	chanPoint *wire.OutPoint) *bolt.Bucket {

	// Retrieve the existing chain bucket for this nursery store.
	chainBucket := tx.Bucket(ns.pfxChainKey)
	if chainBucket == nil {
		return nil
	}

	// Retrieve the existing channel index.
	chanIndex := chainBucket.Bucket(channelIndexKey)
	if chanIndex == nil {
		return nil
	}

	// Serialize the provided channel point and return the bucket matching
	// the serialized key.
	var chanBuffer bytes.Buffer
	if err := writeOutpoint(&chanBuffer, chanPoint); err != nil {
		return nil
	}

	return chanIndex.Bucket(chanBuffer.Bytes())
}

// createHeightBucket creates or retrieves an existing bucket from the height
// index, corresponding to the provided height.
func (ns *nurseryStore) createHeightBucket(tx *bolt.Tx,
	height uint32) (*bolt.Bucket, error) {

	// Ensure that the chain bucket for this nursery store exists.
	chainBucket, err := tx.CreateBucketIfNotExists(ns.pfxChainKey)
	if err != nil {
		return nil, err
	}

	// Ensure that the height index has been properly initialized for this
	// chain.
	hghtIndex, err := chainBucket.CreateBucketIfNotExists(heightIndexKey)
	if err != nil {
		return nil, err
	}

	// Serialize the provided height, as this will form the name of the
	// bucket.
	var heightBytes [4]byte
	byteOrder.PutUint32(heightBytes[:], height)

	// Finally, create or retrieve the bucket in question.
	return hghtIndex.CreateBucketIfNotExists(heightBytes[:])
}

// getHeightBucket retrieves an existing height bucket from the nursery store,
// using the provided block height. If the bucket does not exist, or any bucket
// along its path does not exist, a nil value is returned.
func (ns *nurseryStore) getHeightBucket(tx *bolt.Tx,
	height uint32) *bolt.Bucket {

	// Retrieve the existing chain bucket for this nursery store.
	chainBucket := tx.Bucket(ns.pfxChainKey)
	if chainBucket == nil {
		return nil
	}

	// Retrieve the existing channel index.
	hghtIndex := chainBucket.Bucket(heightIndexKey)
	if hghtIndex == nil {
		return nil
	}

	// Serialize the provided block height and return the bucket matching
	// the serialized key.
	var heightBytes [4]byte
	byteOrder.PutUint32(heightBytes[:], height)

	return hghtIndex.Bucket(heightBytes[:])
}

// createHeightChanBucket creates or retrieves an existing height-channel bucket
// for the provided block height and channel point. This method will attempt to
// instantiate all buckets along the path if required.
func (ns *nurseryStore) createHeightChanBucket(tx *bolt.Tx,
	height uint32, chanPoint *wire.OutPoint) (*bolt.Bucket, error) {

	// Ensure that the height bucket for this nursery store exists.
	hghtBucket, err := ns.createHeightBucket(tx, height)
	if err != nil {
		return nil, err
	}

	// Serialize the provided channel point, as this generates the name of
	// the subdirectory corresponding to the channel of interest.
	var chanBuffer bytes.Buffer
	if err := writeOutpoint(&chanBuffer, chanPoint); err != nil {
		return nil, err
	}
	chanBytes := chanBuffer.Bytes()

	// Finally, create or retrieve an existing height-channel bucket for
	// this channel point.
	return hghtBucket.CreateBucketIfNotExists(chanBytes)
}

// getHeightChanBucket retrieves an existing height-channel bucket from the
// nursery store, using the provided block height and channel point. if the
// bucket does not exist, or any bucket along its path does not exist, a nil
// value is returned.
func (ns *nurseryStore) getHeightChanBucket(tx *bolt.Tx,
	height uint32, chanPoint *wire.OutPoint) *bolt.Bucket {

	// Retrieve the existing height bucket from this nursery store.
	hghtBucket := ns.getHeightBucket(tx, height)
	if hghtBucket == nil {
		return nil
	}

	// Serialize the provided channel point, which generates the key for
	// looking up the proper height-channel bucket inside the height bucket.
	var chanBuffer bytes.Buffer
	if err := writeOutpoint(&chanBuffer, chanPoint); err != nil {
		return nil
	}
	chanBytes := chanBuffer.Bytes()

	// Finally, return the height bucket specified by the serialized channel
	// point.
	return hghtBucket.Bucket(chanBytes)
}

// forEachHeightPrefix enumerates all outputs at the given height whose state
// prefix matches that which is provided. This is used as a subroutine to help
// enumerate crib and kindergarten outputs at a particular height. The callback
// is invoked with serialized bytes retrieved for each output of interest,
// allowing the caller to deserialize them into the appropriate type.
func (ns *nurseryStore) forEachHeightPrefix(prefix []byte, height uint32,
	callback func([]byte) error) error {

	return ns.db.View(func(tx *bolt.Tx) error {
		// Start by retrieving the height bucket corresponding to the
		// provided block height.
		hghtBucket := ns.getHeightBucket(tx, height)
		if hghtBucket == nil {
			return nil
		}

		// Using the height bucket as a starting point, we will traverse
		// its entire two-tier directory structure, and filter for
		// outputs that have the provided prefix. The first layer of the
		// height bucket contains buckets identified by a channel point,
		// thus we first create list of channels contained in this
		// height bucket.
		var channelsAtHeight [][]byte
		if err := hghtBucket.ForEach(func(chanBytes, _ []byte) error {
			channelsAtHeight = append(channelsAtHeight, chanBytes)
			return nil
		}); err != nil {
			return err
		}

		// As we enumerate the outputs referenced in this height bucket,
		// we will need to load the serialized value of each output,
		// which is ultimately stored its respective channel bucket. To
		// do so, we first load the top-level chain bucket, which should
		// already be created if we are at this point.
		chainBucket := tx.Bucket(ns.pfxChainKey)
		if chainBucket == nil {
			return nil
		}

		// Additionally, grab the chain index, which we will facilitate
		// queries for each of the channel buckets of each of the
		// channels in the list we assembled above.
		chanIndex := chainBucket.Bucket(channelIndexKey)
		if chanIndex == nil {
			return nil
		}

		// Now, we are ready to enumerate all outputs with the desired
		// prefix t this block height. We do so by iterating over our
		// list of channels at this height, filtering for outputs in
		// each height-channel bucket that begin with the given prefix,
		// and then retrieving the serialized outputs from the
		// appropriate channel bucket.
		for _, chanBytes := range channelsAtHeight {
			// Retrieve the height-channel bucket for this channel,
			// which holds a sub-bucket for all outputs maturing at
			// this height.
			hghtChanBucket := hghtBucket.Bucket(chanBytes)
			if hghtChanBucket == nil {
				continue
			}

			// Load the appropriate channel bucket from the channel
			// index, this will allow us to retrieve the individual
			// serialized outputs.
			chanBucket := chanIndex.Bucket(chanBytes)
			if chanBucket == nil {
				continue
			}

			// Since all of the outputs of interest will start with
			// the same prefix, we will perform a prefix scan of the
			// buckets contained in the height-channel bucket,
			// efficiently enumerating the desired outputs.
			c := hghtChanBucket.Cursor()

			// Seek to and iterate over all entries starting with
			// the given prefix.
			var pfxOutputKey, _ = c.Seek(prefix)
			for bytes.HasPrefix(pfxOutputKey, prefix) {

				// Use the prefix output key emitted from our
				// scan to load the serialized babyOutput from
				// the appropriate channel bucket.
				outputBytes := chanBucket.Get(pfxOutputKey)
				if outputBytes == nil {
					continue
				}

				// Present the serialized bytes to our call back
				// function, which is responsible for
				// deserializing the bytes into the appropriate
				// type.
				if err := callback(outputBytes); err != nil {
					return err
				}

				// Lastly, advance our prefix output key for the
				// next iteration.
				pfxOutputKey, _ = c.Next()
			}
		}

		return nil
	})
}

var (
	// ErrBucketDoesNotExist signals that a bucket has already been removed,
	// or was never created.
	ErrBucketDoesNotExist = errors.New("bucket does not exist")

	// ErrBucketNotEmpty signals that an attempt to prune a particular
	// bucket failed because it still has active outputs.
	ErrBucketNotEmpty = errors.New("bucket is not empty, cannot be pruned")
)

// deleteAndPruneHeight removes an output from a channel bucket matching the
// provided channel point. The output is assumed top be in the kindergarten
// bucket, since pruning should never occur for any other type of output. If
// after deletion, the channel bucket is empty, this method will attempt to
// delete the bucket as well.
// NOTE: This method returns two concrete errors apart from those returned by
// the underlying database: ErrBucketDoesNotExist and ErrBucketNotEmpty. These
// should be handled in the context of the caller so as they may be benign
// depending on context. Errors returned other than these two should be
// interpreted as database errors.
func (ns *nurseryStore) deleteAndPruneChannel(tx *bolt.Tx,
	chanPoint, outpoint *wire.OutPoint) error {

	// Retrieve the existing chain bucket for this nursery store.
	chainBucket := tx.Bucket(ns.pfxChainKey)
	if chainBucket == nil {
		return nil
	}

	// Retrieve the channel index stored in the chain bucket.
	chanIndex := chainBucket.Bucket(channelIndexKey)
	if chanIndex == nil {
		return nil
	}

	// Serialize the provided channel point, such that we can retrieve the
	// desired channel bucket.
	var chanBuffer bytes.Buffer
	if err := writeOutpoint(&chanBuffer, chanPoint); err != nil {
		return err
	}
	chanBytes := chanBuffer.Bytes()

	// Retrieve the existing channel bucket. If none exists, then our job is
	// complete and it is safe to return nil.
	chanBucket := chanIndex.Bucket(chanBytes)
	if chanBucket == nil {
		return nil
	}

	// Otherwise, the bucket still exists. Serialize the outpoint that needs
	// deletion, prefixing the key with kindergarten prefix. Since all
	// outputs eventually make their way to becoming kindergarten outputs,
	// we can safely assume that they will be stored with a kindergarten
	// prefix.
	pfxOutputBytes, err := prefixOutputKey(kndrPrefix, outpoint)
	if err != nil {
		return err
	}

	// Remove the output in question using the kindergarten-prefixed key we
	// generated above.
	if err := chanBucket.Delete(pfxOutputBytes); err != nil {
		return err
	}

	// Finally, now that the outpoint has been removed from this channel
	// bucket, try to remove the channel bucket altogether if it is now
	// empty.
	return ns.removeBucketIfEmpty(chanIndex, chanBytes)
}

// pruneHeight
// NOTE: This method returns two concrete errors apart from those returned by
// the underlying database: ErrBucketDoesNotExist and ErrBucketNotEmpty. These
// should be handled in the context of the caller so as they may be benign
// depending on context. Errors returned other than these two should be
// interpreted as database errors.
func (ns *nurseryStore) pruneHeight(tx *bolt.Tx, height uint32) error {

	// Fetch the existing chain bucket for this nursery store.
	chainBucket := tx.Bucket(ns.pfxChainKey)
	if chainBucket == nil {
		return nil
	}

	// Load the existing height bucket for the height in question.
	hghtIndex := chainBucket.Bucket(heightIndexKey)
	if hghtIndex == nil {
		return nil
	}

	// Serialize the provided block height, such that it can be used as the
	// key to locate the desired height bucket.
	var heightBytes [4]byte
	byteOrder.PutUint32(heightBytes[:], height)

	// Retrieve the height bucket using the serialized height as the bucket
	// name.
	hghtBucket := hghtIndex.Bucket(heightBytes[:])
	if hghtBucket == nil {
		return ErrBucketDoesNotExist
	}

	// Iterate over the contents of this height bucket, which is comprised
	// of sub-buckets named after the channel points that need attention at
	// this block height. We will attempt to remove each one if they are
	// empty, keeping track of the number of height-channel buckets that
	// still have active outputs.
	var nActiveBuckets int
	if err := hghtBucket.ForEach(func(chanBytes, _ []byte) error {

		// Attempt to each height-channel bucket from the height bucket
		// located above.
		err := ns.removeBucketIfEmpty(hghtBucket, chanBytes)
		switch err {
		case nil:
			// The height-channel bucket was removed successfully!
			return nil

		case ErrBucketDoesNotExist:
			// The height-channel bucket could not be located--no
			// harm, no foul.
			return nil

		case ErrBucketNotEmpty:
			// The bucket still has active outputs at this height,
			// increment our number of still active height-channel
			// buckets.
			nActiveBuckets++
			return nil

		default:
			// Database error!
			return err
		}

	}); err != nil {
		return err
	}

	// If we located any height-channel buckets that still have active
	// outputs, it is unsafe to delete this height bucket. Signal this event
	// to the caller so that they can determine the appropriate action.
	if nActiveBuckets > 0 {
		return ErrBucketNotEmpty
	}

	// All of the height-channel buckets are empty or have been previously
	// removed, proceed by attempting to remove the height bucket
	// altogether.
	return ns.removeBucketIfEmpty(hghtIndex, heightBytes[:])
}

// removeBucketIfEmpty attempts to delete a bucket specified by name from the
// provided parent bucket.
// NOTE: This method returns two concrete errors apart from those returned by
// the underlying database: ErrBucketDoesNotExist and ErrBucketNotEmpty. These
// should be handled in the context of the caller so as they may be benign
// depending on context. Errors returned other than these two should be
// interpreted as database errors.
func (ns *nurseryStore) removeBucketIfEmpty(parent *bolt.Bucket,
	bktName []byte) error {

	// Attempt to fetch the named bucket from its parent.
	bkt := parent.Bucket(bktName)
	if bkt == nil {
		// No bucket was found, signal this to the caller.
		return ErrBucketDoesNotExist
	}

	// The bucket exists, now compute how many children *it* has.
	nChildren, err := ns.numChildrenInBucket(bkt)
	if err != nil {
		return err
	}

	// If the number of children is non-zero, alert the caller that the
	// named bucket is not being removed.
	if nChildren > 0 {
		return ErrBucketNotEmpty
	}

	// Otherwise, remove the empty bucket from its parent.
	return parent.DeleteBucket(bktName)
}

// numChildrenInBucket computes the number of children contained in the given
// boltdb bucket.
func (ns *nurseryStore) numChildrenInBucket(parent *bolt.Bucket) (int, error) {
	var nChildren int
	if err := parent.ForEach(func(_, _ []byte) error {
		nChildren++
		return nil
	}); err != nil {
		return 0, err
	}

	return nChildren, nil
}

// Compile-time constraint to ensure nurseryStore implements NurseryStore.
var _ NurseryStore = (*nurseryStore)(nil)
