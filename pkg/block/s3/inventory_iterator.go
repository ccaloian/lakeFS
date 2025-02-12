package s3

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/treeverse/lakefs/pkg/block"
	"github.com/treeverse/lakefs/pkg/cloud/aws/s3inventory"
	"github.com/treeverse/lakefs/pkg/cmdutils"
)

var ErrInventoryNotSorted = errors.New("got unsorted s3 inventory")

type InventoryIterator struct {
	*Inventory
	err                   error
	val                   *block.InventoryObject
	buffer                []*s3inventory.InventoryObject
	inventoryFileIndex    int
	valIndexInBuffer      int
	inventoryFileProgress *cmdutils.Progress
	currentFileProgress   *cmdutils.Progress
	currentPrefix         int
}

func NewInventoryIterator(inv *Inventory) *InventoryIterator {
	creationTimestamp, err := strconv.ParseInt(inv.Manifest.CreationTimestamp, 10, 64) //nolint: gomnd
	if err != nil {
		inv.logger.Errorf("failed to get creation timestamp from manifest")
		creationTimestamp = 0
	}
	t := time.Unix(creationTimestamp/int64(time.Second/time.Millisecond), 0)
	inventoryFileProgress := cmdutils.NewActiveProgress(fmt.Sprintf("Inventory (%s) Files Read", t.Format("2006-01-02")), cmdutils.Bar)
	inventoryFileProgress.SetTotal(int64(len(inv.Manifest.Files)))
	return &InventoryIterator{
		Inventory:             inv,
		inventoryFileIndex:    -1,
		inventoryFileProgress: inventoryFileProgress,
		currentFileProgress:   cmdutils.NewActiveProgress(fmt.Sprintf("Inventory (%s) Current File", t.Format("2006-01-02")), cmdutils.Bar),
	}
}

func (it *InventoryIterator) Next() bool {
	for {
		if len(it.Manifest.Files) == 0 {
			// empty manifest
			return false
		}
		val := it.nextFromBuffer()
		if val != nil {
			// validate element order
			if it.shouldSort && it.val != nil && val.Key < it.val.Key {
				it.err = ErrInventoryNotSorted
				return false
			}
			it.currentFileProgress.SetCurrent(int64(it.valIndexInBuffer + 1))
			it.val = val
			return true
		}
		// value not found in buffer, need to reload the buffer
		it.valIndexInBuffer = -1
		if !it.moveToNextInventoryFile() {
			// no more files left
			it.inventoryFileProgress.SetCompleted(true)
			it.currentFileProgress.SetCompleted(true)
			return false
		}
		if !it.fillBuffer() {
			return false
		}
	}
}

func (it *InventoryIterator) moveToNextInventoryFile() bool {
	if it.inventoryFileIndex == len(it.Manifest.Files)-1 {
		return false
	}
	it.inventoryFileIndex += 1
	it.inventoryFileProgress.Incr()
	it.logger.Debugf("moving to next manifest file: %s", it.Manifest.Files[it.inventoryFileIndex].Key)
	it.buffer = nil
	return true
}

func (it *InventoryIterator) fillBuffer() bool {
	it.logger.Debug("start reading rows from inventory to buffer")
	rdr, err := it.reader.GetFileReader(it.Manifest.Format, it.Manifest.inventoryBucket, it.Manifest.Files[it.inventoryFileIndex].Key)
	if err != nil {
		it.err = err
		return false
	}
	it.currentFileProgress.SetTotal(rdr.GetNumRows())
	it.currentFileProgress.SetCurrent(0)
	defer func() {
		err = rdr.Close()
		if err != nil {
			it.logger.Errorf("failed to close manifest file reader. file=%s, err=%w", it.Manifest.Files[it.inventoryFileIndex].Key, err)
		}
	}()
	it.buffer, err = rdr.Read(int(rdr.GetNumRows()))
	if err != nil {
		it.err = err
		return false
	}
	return true
}

func (it *InventoryIterator) nextFromBuffer() *block.InventoryObject {
	for i := it.valIndexInBuffer + 1; i < len(it.buffer); i++ {
		obj := it.buffer[i]
		if !obj.IsLatest || obj.IsDeleteMarker {
			continue
		}
		if len(it.prefixes) > 0 {
			// check file against prefix filter
			// loop while current prefix may fit current object
			for it.prefixes[it.currentPrefix] < obj.Key {
				if strings.HasPrefix(obj.Key, it.prefixes[it.currentPrefix]) {
					// found current prefix
					break
				}
				it.currentPrefix++
				if it.currentPrefix == len(it.prefixes) {
					// no more prefixes
					return nil
				}
			}
			if !strings.HasPrefix(obj.Key, it.prefixes[it.currentPrefix]) {
				continue
			}
		}
		res := block.InventoryObject{
			Bucket:          obj.Bucket,
			Key:             obj.Key,
			PhysicalAddress: obj.GetPhysicalAddress(),
			Size:            obj.Size,
			LastModified:    obj.LastModified,
			Checksum:        obj.Checksum,
		}
		it.valIndexInBuffer = i
		return &res
	}
	return nil
}

func (it *InventoryIterator) Err() error {
	return it.err
}

func (it *InventoryIterator) Get() *block.InventoryObject {
	return it.val
}

func (it *InventoryIterator) Progress() []*cmdutils.Progress {
	return []*cmdutils.Progress{
		it.inventoryFileProgress, it.currentFileProgress,
	}
}
