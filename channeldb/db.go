package channeldb

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/boltdb/bolt"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/wire"
)

const (
	dbName           = "channel.db"
	dbFilePermission = 0600
)

// Migration is a function which takes a prior outdated version of the database
// instances and mutates the key/bucket structure to arrive at a more up-to-date
// version of the database.
type migration func(tx *bolt.Tx) error

type version struct {
	number    uint32
	migration migration
}

var (
	// DBVersions is storing all versions of database. If current version of
	// database don't match with latest version this list will be used for
	// retrieving all migration function that are need to apply to the
	// current db.
	DBVersions = []version{
		{
			number:    1,
			migration: nil, // The base DB version requires no migration
		},
	}

	// Big endian is the preferred byte order, due to cursor scans over integer
	// keys iterating in order.
	byteOrder = binary.BigEndian
)

var bufPool = &sync.Pool{
	New: func() interface{} { return new(bytes.Buffer) },
}

// DB is the primary datastore for the LND daemon. The database stores
// information related to nodes, routing data, open/closed channels, fee
// schedules, and reputation data.
type DB struct {
	store     *bolt.DB
	netParams *chaincfg.Params
	dbPath    string
}

// Open opens an existing channeldb created under the passed namespace with
// sensitive data encrypted by the passed EncryptorDecryptor implementation.
// TODO(roasbeef): versioning?
func Open(dbPath string, netParams *chaincfg.Params) (*DB, error) {
	path := filepath.Join(dbPath, dbName)

	if !fileExists(path) {
		if err := createChannelDB(dbPath); err != nil {
			return nil, err
		}
	}

	bdb, err := bolt.Open(path, dbFilePermission, nil)
	if err != nil {
		return nil, err
	}

	return &DB{
		store:     bdb,
		netParams: netParams,
		dbPath:    dbPath,
	}, nil
}

// Wipe completely deletes all saved state within all used buckets within the
// database. The deletion is done in a single transaction, therefore this
// operation is fully atomic.
func (d *DB) Wipe() error {
	return d.store.Update(func(tx *bolt.Tx) error {
		err := tx.DeleteBucket(openChannelBucket)
		if err != nil && err != bolt.ErrBucketNotFound {
			return err
		}

		err = tx.DeleteBucket(closedChannelBucket)
		if err != nil && err != bolt.ErrBucketNotFound {
			return err
		}

		err = tx.DeleteBucket(invoiceBucket)
		if err != nil && err != bolt.ErrBucketNotFound {
			return err
		}

		err = tx.DeleteBucket(nodeInfoBucket)
		if err != nil && err != bolt.ErrBucketNotFound {
			return err
		}

		return nil
	})
}

// Close terminates the underlying database handle manually.
func (d *DB) Close() error {
	return d.store.Close()
}

// createChannelDB creates and initializes a fresh version of channeldb. In
// the case that the target path has not yet been created or doesn't yet exist,
// then the path is created. Additionally, all required top-level buckets used
// within the database are created.
func createChannelDB(dbPath string) error {
	if !fileExists(dbPath) {
		if err := os.MkdirAll(dbPath, 0700); err != nil {
			return err
		}
	}

	path := filepath.Join(dbPath, dbName)
	bdb, err := bolt.Open(path, dbFilePermission, nil)
	if err != nil {
		return err
	}

	err = bdb.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucket(openChannelBucket); err != nil {
			return err
		}

		if _, err := tx.CreateBucket(closedChannelBucket); err != nil {
			return err
		}

		if _, err := tx.CreateBucket(invoiceBucket); err != nil {
			return err
		}

		if _, err := tx.CreateBucket(nodeInfoBucket); err != nil {
			return err
		}

		if _, err := tx.CreateBucket(metaBucket); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("unable to create new channeldb")
	}

	return bdb.Close()
}

// fileExists returns true if the file exists, and false otherwise.
func fileExists(path string) bool {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}

	return true
}

// FetchOpenChannel returns all stored currently active/open channels
// associated with the target nodeID. In the case that no active channels are
// known to have been created with this node, then a zero-length slice is
// returned.
func (d *DB) FetchOpenChannels(nodeID *btcec.PublicKey) ([]*OpenChannel, error) {
	var channels []*OpenChannel
	err := d.store.View(func(tx *bolt.Tx) error {
		// Get the bucket dedicated to storing the meta-data for open
		// channels.
		openChanBucket := tx.Bucket(openChannelBucket)
		if openChanBucket == nil {
			return nil
		}

		// Within this top level bucket, fetch the bucket dedicated to storing
		// open channel data specific to the remote node.
		pub := nodeID.SerializeCompressed()
		nodeChanBucket := openChanBucket.Bucket(pub)
		if nodeChanBucket == nil {
			return nil
		}

		// Finally, we both of the necessary buckets retrieved, fetch
		// all the active channels related to this node.
		nodeChannels, err := d.fetchNodeChannels(openChanBucket,
			nodeChanBucket)
		if err != nil {
			return err
		}

		channels = nodeChannels
		return nil
	})

	return channels, err
}

// fetchNodeChannels retrieves all active channels from the target
// nodeChanBucket. This function is typically used to fetch all the active
// channels related to a particualr node.
func (d *DB) fetchNodeChannels(openChanBucket,
	nodeChanBucket *bolt.Bucket) ([]*OpenChannel, error) {

	var channels []*OpenChannel

	// Once we have the node's channel bucket, iterate through each
	// item in the inner chan ID bucket. This bucket acts as an
	// index for all channels we currently have open with this node.
	nodeChanIDBucket := nodeChanBucket.Bucket(chanIDBucket[:])
	if nodeChanIDBucket == nil {
		return nil, nil
	}
	err := nodeChanIDBucket.ForEach(func(k, v []byte) error {
		if k == nil {
			return nil
		}

		outBytes := bytes.NewReader(k)
		chanID := &wire.OutPoint{}
		if err := readOutpoint(outBytes, chanID); err != nil {
			return err
		}

		oChannel, err := fetchOpenChannel(openChanBucket,
			nodeChanBucket, chanID)
		if err != nil {
			return err
		}
		oChannel.Db = d

		channels = append(channels, oChannel)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return channels, nil
}

// FetchAllChannels attempts to retrieve all open channels currently stored
// within the database. If no active channels exist within the network, then
// ErrNoActiveChannels is returned.
func (d *DB) FetchAllChannels() ([]*OpenChannel, error) {
	var channels []*OpenChannel

	err := d.store.View(func(tx *bolt.Tx) error {
		// Get the bucket dedicated to storing the meta-data for open
		// channels.
		openChanBucket := tx.Bucket(openChannelBucket)
		if openChanBucket == nil {
			return ErrNoActiveChannels
		}

		// Next, fetch the bucket dedicated to storing meta-data
		// related to all nodes. All keys within this bucket are the
		// serialized public keys of all our direct counterparties.
		nodeMetaBucket := tx.Bucket(nodeInfoBucket)
		if nodeMetaBucket == nil {
			return fmt.Errorf("node bucket not created")
		}

		// Finally for each node public key in the bucket, fetch all
		// the channels related to this particualr ndoe.
		return nodeMetaBucket.ForEach(func(k, v []byte) error {
			nodeChanBucket := openChanBucket.Bucket(k)
			if nodeChanBucket == nil {
				return nil
			}

			nodeChannels, err := d.fetchNodeChannels(openChanBucket,
				nodeChanBucket)
			if err != nil {
				return err
			}

			channels = append(channels, nodeChannels...)
			return nil
		})
	})

	return channels, err
}

// SyncVersions function is used for safe db version synchronization. It applies
// migration functions to the current database and recovers the previous
// state of db if at least one error/panic appeared during migration.
func (d *DB) SyncVersions(versions []version) error {
	meta, err := d.FetchMeta(nil)
	if err != nil {
		return err
	}

	latestVersion := getLatestDBVersion(versions)

	if meta.dbVersionNumber < latestVersion {
		migrations := getMigrationsToApply(versions, meta.dbVersionNumber)

		return d.store.Update(func(tx *bolt.Tx) error {
			for _, migration := range migrations {
				if migration == nil {
					continue
				}

				if err := migration(tx); err != nil {
					return err
				}
			}

			meta.dbVersionNumber = latestVersion
			if err := d.PutMeta(meta, tx); err != nil {
				return err
			}

			return nil
		})

	}

	return nil
}

func getLatestDBVersion(versions []version) uint32 {
	return versions[len(versions)-1].number
}

// getMigrationsToApply retrieves the migration function that should be
// applied to the database.
func getMigrationsToApply(versions []version, version uint32) []migration {
	migrations := make([]migration, 0, len(versions))

	for _, v := range versions {
		if v.number > version {
			migrations = append(migrations, v.migration)
		}
	}

	return migrations
}
