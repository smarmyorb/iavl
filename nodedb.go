package iavl

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/pkg/errors"
	dbm "github.com/tendermint/tm-db"
)

const (
	int64Size         = 8
	hashSize          = sha256.Size
	genesisVersion    = 1
	storageVersionKey = "storage_version"
	// We store latest saved version together with storage version delimited by the constant below.
	// This delimiter is valid only if fast storage is enabled (i.e. storageVersion >= fastStorageVersionValue).
	// The latest saved version is needed for protection against downgrade and re-upgrade. In such a case, it would
	// be possible to observe mismatch between the latest version state and the fast nodes on disk.
	// Therefore, we would like to detect that and overwrite fast nodes on disk with the latest version state.
	fastStorageVersionDelimiter = "-"
	// Using semantic versioning: https://semver.org/
	defaultStorageVersionValue = "1.0.0"
	fastStorageVersionValue    = "1.1.0"
)

var (
	// All node keys are prefixed with the byte 'n'. This ensures no collision is
	// possible with the other keys, and makes them easier to traverse. They are indexed by the node hash.
	nodeKeyFormat = NewKeyFormat('n', hashSize) // n<hash>

	// Orphans are keyed in the database by their expected lifetime.
	// The first number represents the *last* version at which the orphan needs
	// to exist, while the second number represents the *earliest* version at
	// which it is expected to exist - which starts out by being the version
	// of the node being orphaned.
	// To clarify:
	// When I write to key {X} with value V and old value O, we orphan O with <last-version>=time of write
	// and <first-version> = version O was created at.
	orphanKeyFormat = NewKeyFormat('o', int64Size, int64Size, hashSize) // o<last-version><first-version><hash>

	// Key Format for making reads and iterates go through a data-locality preserving db.
	// The value at an entry will list what version it was written to.
	// Then to query values, you first query state via this fast method.
	// If its present, then check the tree version. If tree version >= result_version,
	// return result_version. Else, go through old (slow) IAVL get method that walks through tree.
	fastKeyFormat = NewKeyFormat('f', 0) // f<keystring>

	// Key Format for storing metadata about the chain such as the vesion number.
	// The value at an entry will be in a variable format and up to the caller to
	// decide how to parse.
	metadataKeyFormat = NewKeyFormat('m', 0) // v<keystring>

	// Root nodes are indexed separately by their version
	rootKeyFormat = NewKeyFormat('r', int64Size) // r<version>
)

var (
	errInvalidFastStorageVersion = fmt.Sprintf("Fast storage version must be in the format <storage version>%s<latest fast cache version>", fastStorageVersionDelimiter)
)

type nodeDB struct {
	mtx            sync.Mutex       // Read/write lock.
	db             dbm.DB           // Persistent node storage.
	batch          dbm.Batch        // Batched writing buffer.
	opts           Options          // Options to customize for pruning/writing
	versionReaders map[int64]uint32 // Number of active version readers
	storageVersion string           // Storage version

	latestVersion  int64
	nodeCache      map[string]*list.Element // Node cache.
	nodeCacheSize  int                      // Node cache size limit in elements.
	nodeCacheQueue *list.List               // LRU queue of cache elements. Used for deletion.

	fastNodeCache      map[string]*list.Element // FastNode cache.
	fastNodeCacheSize  int                      // FastNode cache size limit in elements.
	fastNodeCacheQueue *list.List               // LRU queue of cache elements. Used for deletion.
}

func newNodeDB(db dbm.DB, cacheSize int, opts *Options) *nodeDB {
	if opts == nil {
		o := DefaultOptions()
		opts = &o
	}

	storeVersion, err := db.Get(metadataKeyFormat.Key([]byte(storageVersionKey)))

	if err != nil || storeVersion == nil {
		storeVersion = []byte(defaultStorageVersionValue)
	}

	return &nodeDB{
		db:                 db,
		batch:              db.NewBatch(),
		opts:               *opts,
		latestVersion:      0, // initially invalid
		nodeCache:          make(map[string]*list.Element),
		nodeCacheSize:      cacheSize,
		nodeCacheQueue:     list.New(),
		fastNodeCache:      make(map[string]*list.Element),
		fastNodeCacheSize:  cacheSize,
		fastNodeCacheQueue: list.New(),
		versionReaders:     make(map[int64]uint32, 8),
		storageVersion:     string(storeVersion),
	}
}

// GetNode gets a node from memory or disk. If it is an inner node, it does not
// load its children.
func (ndb *nodeDB) GetNode(hash []byte) *Node {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()

	if len(hash) == 0 {
		panic("nodeDB.GetNode() requires hash")
	}

	// Check the cache.
	if elem, ok := ndb.nodeCache[string(hash)]; ok {
		// Already exists. Move to back of nodeCacheQueue.
		ndb.nodeCacheQueue.MoveToBack(elem)
		return elem.Value.(*Node)
	}

	// Doesn't exist, load.
	buf, err := ndb.db.Get(ndb.nodeKey(hash))
	if err != nil {
		panic(fmt.Sprintf("can't get node %X: %v", hash, err))
	}
	if buf == nil {
		panic(fmt.Sprintf("Value missing for hash %x corresponding to nodeKey %x", hash, ndb.nodeKey(hash)))
	}

	node, err := MakeNode(buf)
	if err != nil {
		panic(fmt.Sprintf("Error reading Node. bytes: %x, error: %v", buf, err))
	}

	node.hash = hash
	node.persisted = true
	ndb.cacheNode(node)

	return node
}

func (ndb *nodeDB) GetFastNode(key []byte) (*FastNode, error) {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()
	if !ndb.hasUpgradedToFastStorage() {
		return nil, errors.New("storage version is not fast")
	}

	if len(key) == 0 {
		return nil, fmt.Errorf("nodeDB.GetFastNode() requires key, len(key) equals 0")
	}

	// Check the cache.
	if elem, ok := ndb.fastNodeCache[string(key)]; ok {
		// Already exists. Move to back of fastNodeCacheQueue.
		ndb.fastNodeCacheQueue.MoveToBack(elem)
		return elem.Value.(*FastNode), nil
	}

	// Doesn't exist, load.
	buf, err := ndb.db.Get(ndb.fastNodeKey(key))
	if err != nil {
		return nil, fmt.Errorf("can't get FastNode %X: %w", key, err)
	}
	if buf == nil {
		return nil, nil
	}

	fastNode, err := DeserializeFastNode(key, buf)
	if err != nil {
		return nil, fmt.Errorf("error reading FastNode. bytes: %x, error: %w", buf, err)
	}

	ndb.cacheFastNode(fastNode)
	return fastNode, nil
}

// SaveNode saves a node to disk.
func (ndb *nodeDB) SaveNode(node *Node) {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()

	if node.hash == nil {
		panic("Expected to find node.hash, but none found.")
	}
	if node.persisted {
		panic("Shouldn't be calling save on an already persisted node.")
	}

	// Save node bytes to db.
	var buf bytes.Buffer
	buf.Grow(node.encodedSize())

	if err := node.writeBytes(&buf); err != nil {
		panic(err)
	}

	if err := ndb.batch.Set(ndb.nodeKey(node.hash), buf.Bytes()); err != nil {
		panic(err)
	}
	debug("BATCH SAVE %X %p\n", node.hash, node)
	node.persisted = true
	ndb.cacheNode(node)
}

// SaveNode saves a FastNode to disk and add to cache.
func (ndb *nodeDB) SaveFastNode(node *FastNode) error {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()
	return ndb.saveFastNodeUnlocked(node, true)
}

// SaveNode saves a FastNode to disk without adding to cache.
func (ndb *nodeDB) SaveFastNodeNoCache(node *FastNode) error {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()
	return ndb.saveFastNodeUnlocked(node, false)
}

// setFastStorageVersionToBatch sets storage version to fast where the version is
// 1.1.0-<version of the current live state>. Returns error if storage version is incorrect or on
// db error, nil otherwise. Requires changes to be comitted after to be persisted.
func (ndb *nodeDB) setFastStorageVersionToBatch() error {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()

	var newVersion string
	if ndb.storageVersion >= fastStorageVersionValue {
		// Storage version should be at index 0 and latest fast cache version at index 1
		versions := strings.Split(ndb.storageVersion, fastStorageVersionDelimiter)

		if len(versions) > 2 {
			return errors.New(errInvalidFastStorageVersion)
		}

		newVersion = versions[0]
	} else {
		newVersion = fastStorageVersionValue
	}

	newVersion += fastStorageVersionDelimiter + strconv.Itoa(int(ndb.getLatestVersion()))

	if err := ndb.batch.Set(metadataKeyFormat.Key([]byte(storageVersionKey)), []byte(newVersion)); err != nil {
		return err
	}
	ndb.storageVersion = newVersion
	return nil
}

func (ndb *nodeDB) getStorageVersion() string {
	return ndb.storageVersion
}

// Returns true if the upgrade to latest storage version has been performed, false otherwise.
func (ndb *nodeDB) hasUpgradedToFastStorage() bool {
	return ndb.getStorageVersion() >= fastStorageVersionValue
}

// Returns true if the upgrade to fast storage has occurred but it does not match the live state, false otherwise.
// When the live state is not matched, we must force reupgrade.
// We determine this by checking the version of the live state and the version of the live state when
// latest storage was updated on disk the last time.
func (ndb *nodeDB) shouldForceFastStorageUpgrade() bool {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()
	versions := strings.Split(ndb.storageVersion, fastStorageVersionDelimiter)

	if len(versions) == 2 {
		if versions[1] != strconv.Itoa(int(ndb.getLatestVersion())) {
			return true
		}
	}
	return false
}

// SaveNode saves a FastNode to disk.
// CONTRACT: the caller must serizlize access to this method through ndb.mtx.
func (ndb *nodeDB) saveFastNodeUnlocked(node *FastNode, shouldAddToCache bool) error {
	if node.key == nil {
		return fmt.Errorf("FastNode cannot have a nil value for key")
	}

	// Save node bytes to db.
	var buf bytes.Buffer
	buf.Grow(node.encodedSize())

	if err := node.writeBytes(&buf); err != nil {
		return fmt.Errorf("error while writing fastnode bytes. Err: %w", err)
	}

	if err := ndb.batch.Set(ndb.fastNodeKey(node.key), buf.Bytes()); err != nil {
		return fmt.Errorf("error while writing key/val to nodedb batch. Err: %w", err)
	}
	if shouldAddToCache {
		ndb.cacheFastNode(node)
	}
	return nil
}

// Has checks if a hash exists in the database.
func (ndb *nodeDB) Has(hash []byte) (bool, error) {
	key := ndb.nodeKey(hash)

	if ldb, ok := ndb.db.(*dbm.GoLevelDB); ok {
		exists, err := ldb.DB().Has(key, nil)
		if err != nil {
			return false, err
		}
		return exists, nil
	}
	value, err := ndb.db.Get(key)
	if err != nil {
		return false, err
	}

	return value != nil, nil
}

// SaveBranch saves the given node and all of its descendants.
// NOTE: This function clears leftNode/rigthNode recursively and
// calls _hash() on the given node.
// TODO refactor, maybe use hashWithCount() but provide a callback.
func (ndb *nodeDB) SaveBranch(node *Node) []byte {
	if node.persisted {
		return node.hash
	}

	if node.leftNode != nil {
		node.leftHash = ndb.SaveBranch(node.leftNode)
	}
	if node.rightNode != nil {
		node.rightHash = ndb.SaveBranch(node.rightNode)
	}

	node._hash()
	ndb.SaveNode(node)

	// resetBatch only working on generate a genesis block
	if node.version <= genesisVersion {
		ndb.resetBatch()
	}
	node.leftNode = nil
	node.rightNode = nil

	return node.hash
}

// resetBatch reset the db batch, keep low memory used
func (ndb *nodeDB) resetBatch() error {
	var err error
	if ndb.opts.Sync {
		err = ndb.batch.WriteSync()
	} else {
		err = ndb.batch.Write()
	}
	if err != nil {
		return err
	}
	err = ndb.batch.Close()
	if err != nil {
		return err
	}

	ndb.batch = ndb.db.NewBatch()

	return nil
}

// DeleteVersion deletes a tree version from disk.
// calls deleteOrphans(version), deleteRoot(version, checkLatestVersion)
func (ndb *nodeDB) DeleteVersion(version int64, checkLatestVersion bool) error {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()

	if ndb.versionReaders[version] > 0 {
		return errors.Errorf("unable to delete version %v, it has %v active readers", version, ndb.versionReaders[version])
	}

	err := ndb.deleteOrphans(version)
	if err != nil {
		return err
	}

	err = ndb.deleteRoot(version, checkLatestVersion)
	if err != nil {
		return err
	}
	return err
}

// DeleteVersionsFrom permanently deletes all tree versions from the given version upwards.
func (ndb *nodeDB) DeleteVersionsFrom(version int64) error {
	latest := ndb.getLatestVersion()
	if latest < version {
		return nil
	}
	root, err := ndb.getRoot(latest)
	if err != nil {
		return err
	}
	if root == nil {
		return errors.Errorf("root for version %v not found", latest)
	}

	for v, r := range ndb.versionReaders {
		if v >= version && r != 0 {
			return errors.Errorf("unable to delete version %v with %v active readers", v, r)
		}
	}

	// First, delete all active nodes in the current (latest) version whose node version is after
	// the given version.
	err = ndb.deleteNodesFrom(version, root)
	if err != nil {
		return err
	}

	// Next, delete orphans:
	// - Delete orphan entries *and referred nodes* with fromVersion >= version
	// - Delete orphan entries with toVersion >= version-1 (since orphans at latest are not orphans)
	err = ndb.traverseOrphans(func(key, hash []byte) error {
		var fromVersion, toVersion int64
		orphanKeyFormat.Scan(key, &toVersion, &fromVersion)

		if fromVersion >= version {
			if err = ndb.batch.Delete(key); err != nil {
				return err
			}
			if err = ndb.batch.Delete(ndb.nodeKey(hash)); err != nil {
				return err
			}
		} else if toVersion >= version-1 {
			if err := ndb.batch.Delete(key); err != nil {
				return err
			}
		}
		return nil
	})

	if err != nil {
		return err
	}

	// Delete the version root entries
	err = ndb.traverseRange(rootKeyFormat.Key(version), rootKeyFormat.Key(int64(math.MaxInt64)), func(k, v []byte) error {
		if err := ndb.batch.Delete(k); err != nil {
			return err
		}
		return nil
	})

	if err != nil {
		return err
	}

	// Delete fast node entries
	err = ndb.traverseFastNodes(func(keyWithPrefix, v []byte) error {
		key := keyWithPrefix[1:]
		fastNode, err := DeserializeFastNode(key, v)

		if err != nil {
			return err
		}

		if version <= fastNode.versionLastUpdatedAt {
			if err := ndb.DeleteFastNode(fastNode.key); err != nil {
				return err
			}
		}
		return nil
	})

	if err != nil {
		return err
	}

	return nil
}

// DeleteVersionsRange deletes versions from an interval (not inclusive).
func (ndb *nodeDB) DeleteVersionsRange(fromVersion, toVersion int64) error {
	if fromVersion >= toVersion {
		return errors.New("toVersion must be greater than fromVersion")
	}
	if toVersion == 0 {
		return errors.New("toVersion must be greater than 0")
	}

	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()

	latest := ndb.getLatestVersion()
	if latest < toVersion {
		return errors.Errorf("cannot delete latest saved version (%d)", latest)
	}

	predecessor := ndb.getPreviousVersion(fromVersion)

	for v, r := range ndb.versionReaders {
		if v < toVersion && v > predecessor && r != 0 {
			return errors.Errorf("unable to delete version %v with %v active readers", v, r)
		}
	}

	// If the predecessor is earlier than the beginning of the lifetime, we can delete the orphan.
	// Otherwise, we shorten its lifetime, by moving its endpoint to the predecessor version.
	for version := fromVersion; version < toVersion; version++ {
		err := ndb.traverseOrphansVersion(version, func(key, hash []byte) error {
			var from, to int64
			orphanKeyFormat.Scan(key, &to, &from)
			if err := ndb.batch.Delete(key); err != nil {
				debug("%v\n", err)
				return err
			}
			if from > predecessor {
				if err := ndb.batch.Delete(ndb.nodeKey(hash)); err != nil {
					panic(err)
				}
				ndb.uncacheNode(hash)
				ndb.uncacheFastNode(key)
			} else {
				ndb.saveOrphan(hash, from, predecessor)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	for key, elem := range ndb.fastNodeCache {
		fastNode := elem.Value.(*FastNode)
		if fastNode.versionLastUpdatedAt >= fromVersion && fastNode.versionLastUpdatedAt < toVersion {
			ndb.fastNodeCacheQueue.Remove(elem)
			delete(ndb.fastNodeCache, string(key))
		}
	}

	// Delete the version root entries
	err := ndb.traverseRange(rootKeyFormat.Key(fromVersion), rootKeyFormat.Key(toVersion), func(k, v []byte) error {
		if err := ndb.batch.Delete(k); err != nil {
			return err
		}
		return nil
	})

	if err != nil {
		return err
	}
	return nil
}

func (ndb *nodeDB) DeleteFastNode(key []byte) error {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()
	if err := ndb.batch.Delete(ndb.fastNodeKey(key)); err != nil {
		return err
	}
	ndb.uncacheFastNode(key)
	return nil
}

// deleteNodesFrom deletes the given node and any descendants that have versions after the given
// (inclusive). It is mainly used via LoadVersionForOverwriting, to delete the current version.
func (ndb *nodeDB) deleteNodesFrom(version int64, hash []byte) error {
	if len(hash) == 0 {
		return nil
	}

	node := ndb.GetNode(hash)
	if node.leftHash != nil {
		if err := ndb.deleteNodesFrom(version, node.leftHash); err != nil {
			return err
		}
	}
	if node.rightHash != nil {
		if err := ndb.deleteNodesFrom(version, node.rightHash); err != nil {
			return err
		}
	}

	if node.version >= version {
		if err := ndb.batch.Delete(ndb.nodeKey(hash)); err != nil {
			return err
		}

		ndb.uncacheNode(hash)
	}

	return nil
}

// Saves orphaned nodes to disk under a special prefix.
// version: the new version being saved.
// orphans: the orphan nodes created since version-1
func (ndb *nodeDB) SaveOrphans(version int64, orphans map[string]int64) {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()

	toVersion := ndb.getPreviousVersion(version)
	for hash, fromVersion := range orphans {
		debug("SAVEORPHAN %v-%v %X\n", fromVersion, toVersion, hash)
		ndb.saveOrphan([]byte(hash), fromVersion, toVersion)
	}
}

// Saves a single orphan to disk.
func (ndb *nodeDB) saveOrphan(hash []byte, fromVersion, toVersion int64) {
	if fromVersion > toVersion {
		panic(fmt.Sprintf("Orphan expires before it comes alive.  %d > %d", fromVersion, toVersion))
	}
	key := ndb.orphanKey(fromVersion, toVersion, hash)
	if err := ndb.batch.Set(key, hash); err != nil {
		panic(err)
	}
}

// deleteOrphans deletes orphaned nodes from disk, and the associated orphan
// entries.
func (ndb *nodeDB) deleteOrphans(version int64) error {
	// Will be zero if there is no previous version.
	predecessor := ndb.getPreviousVersion(version)

	// Traverse orphans with a lifetime ending at the version specified.
	// TODO optimize.
	return ndb.traverseOrphansVersion(version, func(key, hash []byte) error {
		var fromVersion, toVersion int64

		// See comment on `orphanKeyFmt`. Note that here, `version` and
		// `toVersion` are always equal.
		orphanKeyFormat.Scan(key, &toVersion, &fromVersion)

		// Delete orphan key and reverse-lookup key.
		if err := ndb.batch.Delete(key); err != nil {
			return err
		}

		// If there is no predecessor, or the predecessor is earlier than the
		// beginning of the lifetime (ie: negative lifetime), or the lifetime
		// spans a single version and that version is the one being deleted, we
		// can delete the orphan.  Otherwise, we shorten its lifetime, by
		// moving its endpoint to the previous version.
		if predecessor < fromVersion || fromVersion == toVersion {
			debug("DELETE predecessor:%v fromVersion:%v toVersion:%v %X\n", predecessor, fromVersion, toVersion, hash)
			if err := ndb.batch.Delete(ndb.nodeKey(hash)); err != nil {
				return err
			}
			ndb.uncacheNode(hash)
		} else {
			debug("MOVE predecessor:%v fromVersion:%v toVersion:%v %X\n", predecessor, fromVersion, toVersion, hash)
			ndb.saveOrphan(hash, fromVersion, predecessor)
		}
		return nil
	})
}

func (ndb *nodeDB) nodeKey(hash []byte) []byte {
	return nodeKeyFormat.KeyBytes(hash)
}

func (ndb *nodeDB) fastNodeKey(key []byte) []byte {
	return fastKeyFormat.KeyBytes(key)
}

func (ndb *nodeDB) orphanKey(fromVersion, toVersion int64, hash []byte) []byte {
	return orphanKeyFormat.Key(toVersion, fromVersion, hash)
}

func (ndb *nodeDB) rootKey(version int64) []byte {
	return rootKeyFormat.Key(version)
}

func (ndb *nodeDB) getLatestVersion() int64 {
	if ndb.latestVersion == 0 {
		ndb.latestVersion = ndb.getPreviousVersion(1<<63 - 1)
	}
	return ndb.latestVersion
}

func (ndb *nodeDB) updateLatestVersion(version int64) {
	if ndb.latestVersion < version {
		ndb.latestVersion = version
	}
}

func (ndb *nodeDB) resetLatestVersion(version int64) {
	ndb.latestVersion = version
}

func (ndb *nodeDB) getPreviousVersion(version int64) int64 {
	itr, err := ndb.db.ReverseIterator(
		rootKeyFormat.Key(1),
		rootKeyFormat.Key(version),
	)
	if err != nil {
		panic(err)
	}
	defer itr.Close()

	pversion := int64(-1)
	for ; itr.Valid(); itr.Next() {
		k := itr.Key()
		rootKeyFormat.Scan(k, &pversion)
		return pversion
	}

	if err := itr.Error(); err != nil {
		panic(err)
	}

	return 0
}

// deleteRoot deletes the root entry from disk, but not the node it points to.
func (ndb *nodeDB) deleteRoot(version int64, checkLatestVersion bool) error {
	if checkLatestVersion && version == ndb.getLatestVersion() {
		return errors.New("Tried to delete latest version")
	}
	if err := ndb.batch.Delete(ndb.rootKey(version)); err != nil {
		return err
	}
	return nil
}

// Traverse orphans and return error if any, nil otherwise
func (ndb *nodeDB) traverseOrphans(fn func(keyWithPrefix, v []byte) error) error {
	return ndb.traversePrefix(orphanKeyFormat.Key(), fn)
}

// Traverse fast nodes and return error if any, nil otherwise
func (ndb *nodeDB) traverseFastNodes(fn func(k, v []byte) error) error {
	return ndb.traversePrefix(fastKeyFormat.Key(), fn)
}

// Traverse orphans ending at a certain version. return error if any, nil otherwise
func (ndb *nodeDB) traverseOrphansVersion(version int64, fn func(k, v []byte) error) error {
	return ndb.traversePrefix(orphanKeyFormat.Key(version), fn)
}

// Traverse all keys and return error if any, nil otherwise
func (ndb *nodeDB) traverse(fn func(key, value []byte) error) error {
	return ndb.traverseRange(nil, nil, fn)
}

// Traverse all keys between a given range (excluding end) and return error if any, nil otherwise
func (ndb *nodeDB) traverseRange(start []byte, end []byte, fn func(k, v []byte) error) error {
	itr, err := ndb.db.Iterator(start, end)
	if err != nil {
		return err
	}
	defer itr.Close()

	for ; itr.Valid(); itr.Next() {
		if err := fn(itr.Key(), itr.Value()); err != nil {
			return err
		}
	}

	if err := itr.Error(); err != nil {
		return err
	}

	return nil
}

// Traverse all keys with a certain prefix. Return error if any, nil otherwise
func (ndb *nodeDB) traversePrefix(prefix []byte, fn func(k, v []byte) error) error {
	itr, err := dbm.IteratePrefix(ndb.db, prefix)
	if err != nil {
		return err
	}
	defer itr.Close()

	for ; itr.Valid(); itr.Next() {
		if err := fn(itr.Key(), itr.Value()); err != nil {
			return err
		}
	}

	return nil
}

// Get iterator for fast prefix and error, if any
func (ndb *nodeDB) getFastIterator(start, end []byte, ascending bool) (dbm.Iterator, error) {
	var startFormatted, endFormatted []byte = nil, nil

	if start != nil {
		startFormatted = fastKeyFormat.KeyBytes(start)
	} else {
		startFormatted = fastKeyFormat.Key()
	}

	if end != nil {
		endFormatted = fastKeyFormat.KeyBytes(end)
	} else {
		endFormatted = fastKeyFormat.Key()
		endFormatted[0]++
	}

	if ascending {
		return ndb.db.Iterator(startFormatted, endFormatted)
	}

	return ndb.db.ReverseIterator(startFormatted, endFormatted)
}

func (ndb *nodeDB) uncacheNode(hash []byte) {
	if elem, ok := ndb.nodeCache[string(hash)]; ok {
		ndb.nodeCacheQueue.Remove(elem)
		delete(ndb.nodeCache, string(hash))
	}
}

// Add a node to the cache and pop the least recently used node if we've
// reached the cache size limit.
func (ndb *nodeDB) cacheNode(node *Node) {
	elem := ndb.nodeCacheQueue.PushBack(node)
	ndb.nodeCache[string(node.hash)] = elem

	if ndb.nodeCacheQueue.Len() > ndb.nodeCacheSize {
		oldest := ndb.nodeCacheQueue.Front()
		hash := ndb.nodeCacheQueue.Remove(oldest).(*Node).hash
		delete(ndb.nodeCache, string(hash))
	}
}

// CONTRACT: the caller must serizlize access to this method through ndb.mtx.
func (ndb *nodeDB) uncacheFastNode(key []byte) {
	if elem, ok := ndb.fastNodeCache[string(key)]; ok {
		ndb.fastNodeCacheQueue.Remove(elem)
		delete(ndb.fastNodeCache, string(key))
	}
}

// Add a node to the cache and pop the least recently used node if we've
// reached the cache size limit.
// CONTRACT: the caller must serizlize access to this method through ndb.mtx.
func (ndb *nodeDB) cacheFastNode(node *FastNode) {
	elem := ndb.fastNodeCacheQueue.PushBack(node)
	ndb.fastNodeCache[string(node.key)] = elem

	if ndb.fastNodeCacheQueue.Len() > ndb.fastNodeCacheSize {
		oldest := ndb.fastNodeCacheQueue.Front()
		key := ndb.fastNodeCacheQueue.Remove(oldest).(*FastNode).key
		delete(ndb.fastNodeCache, string(key))
	}
}

// Write to disk.
func (ndb *nodeDB) Commit() error {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()

	var err error
	if ndb.opts.Sync {
		err = ndb.batch.WriteSync()
	} else {
		err = ndb.batch.Write()
	}
	if err != nil {
		return errors.Wrap(err, "failed to write batch")
	}

	ndb.batch.Close()
	ndb.batch = ndb.db.NewBatch()

	return nil
}

func (ndb *nodeDB) HasRoot(version int64) (bool, error) {
	return ndb.db.Has(ndb.rootKey(version))
}

func (ndb *nodeDB) getRoot(version int64) ([]byte, error) {
	return ndb.db.Get(ndb.rootKey(version))
}

func (ndb *nodeDB) getRoots() (map[int64][]byte, error) {
	roots := map[int64][]byte{}

	ndb.traversePrefix(rootKeyFormat.Key(), func(k, v []byte) error {
		var version int64
		rootKeyFormat.Scan(k, &version)
		roots[version] = v
		return nil
	})
	return roots, nil
}

// SaveRoot creates an entry on disk for the given root, so that it can be
// loaded later.
func (ndb *nodeDB) SaveRoot(root *Node, version int64) error {
	if len(root.hash) == 0 {
		panic("SaveRoot: root hash should not be empty")
	}
	return ndb.saveRoot(root.hash, version)
}

// SaveEmptyRoot creates an entry on disk for an empty root.
func (ndb *nodeDB) SaveEmptyRoot(version int64) error {
	return ndb.saveRoot([]byte{}, version)
}

func (ndb *nodeDB) saveRoot(hash []byte, version int64) error {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()

	// We allow the initial version to be arbitrary
	latest := ndb.getLatestVersion()
	if latest > 0 && version != latest+1 {
		return fmt.Errorf("must save consecutive versions; expected %d, got %d", latest+1, version)
	}

	if err := ndb.batch.Set(ndb.rootKey(version), hash); err != nil {
		return err
	}

	ndb.updateLatestVersion(version)

	return nil
}

func (ndb *nodeDB) incrVersionReaders(version int64) {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()
	ndb.versionReaders[version]++
}

func (ndb *nodeDB) decrVersionReaders(version int64) {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()
	if ndb.versionReaders[version] > 0 {
		ndb.versionReaders[version]--
	}
}

// Utility and test functions

func (ndb *nodeDB) leafNodes() ([]*Node, error) {
	leaves := []*Node{}

	err := ndb.traverseNodes(func(hash []byte, node *Node) error {
		if node.isLeaf() {
			leaves = append(leaves, node)
		}
		return nil
	})

	if err != nil {
		return nil, err
	}

	return leaves, nil
}

func (ndb *nodeDB) nodes() ([]*Node, error) {
	nodes := []*Node{}

	err := ndb.traverseNodes(func(hash []byte, node *Node) error {
		nodes = append(nodes, node)
		return nil
	})

	if err != nil {
		return nil, err
	}

	return nodes, nil
}

func (ndb *nodeDB) orphans() ([][]byte, error) {
	orphans := [][]byte{}

	err := ndb.traverseOrphans(func(k, v []byte) error {
		orphans = append(orphans, v)
		return nil
	})

	if err != nil {
		return nil, err
	}

	return orphans, nil
}

func (ndb *nodeDB) roots() map[int64][]byte {
	roots, _ := ndb.getRoots()
	return roots
}

// Not efficient.
// NOTE: DB cannot implement Size() because
// mutations are not always synchronous.
func (ndb *nodeDB) size() int {
	size := 0
	err := ndb.traverse(func(k, v []byte) error {
		size++
		return nil
	})

	if err != nil {
		return -1
	}
	return size
}

func (ndb *nodeDB) traverseNodes(fn func(hash []byte, node *Node) error) error {
	nodes := []*Node{}

	err := ndb.traversePrefix(nodeKeyFormat.Key(), func(key, value []byte) error {
		node, err := MakeNode(value)
		if err != nil {
			return err
		}
		nodeKeyFormat.Scan(key, &node.hash)
		nodes = append(nodes, node)
		return nil
	})

	if err != nil {
		return err
	}

	sort.Slice(nodes, func(i, j int) bool {
		return bytes.Compare(nodes[i].key, nodes[j].key) < 0
	})

	for _, n := range nodes {
		if err := fn(n.hash, n); err != nil {
			return err
		}
	}
	return nil
}

func (ndb *nodeDB) String() (string, error) {
	var str string
	index := 0

	ndb.traversePrefix(rootKeyFormat.Key(), func(key, value []byte) error {
		str += fmt.Sprintf("%s: %x\n", string(key), value)
		return nil
	})
	str += "\n"

	err := ndb.traverseOrphans(func(key, value []byte) error {
		str += fmt.Sprintf("%s: %x\n", string(key), value)
		return nil
	})

	if err != nil {
		return "", err
	}

	str += "\n"

	err = ndb.traverseNodes(func(hash []byte, node *Node) error {
		switch {
		case len(hash) == 0:
			str += "<nil>\n"
		case node == nil:
			str += fmt.Sprintf("%s%40x: <nil>\n", nodeKeyFormat.Prefix(), hash)
		case node.value == nil && node.height > 0:
			str += fmt.Sprintf("%s%40x: %s   %-16s h=%d version=%d\n",
				nodeKeyFormat.Prefix(), hash, node.key, "", node.height, node.version)
		default:
			str += fmt.Sprintf("%s%40x: %s = %-16s h=%d version=%d\n",
				nodeKeyFormat.Prefix(), hash, node.key, node.value, node.height, node.version)
		}
		index++
		return nil
	})

	if err != nil {
		return "", err
	}

	return "-" + "\n" + str + "-", nil
}
