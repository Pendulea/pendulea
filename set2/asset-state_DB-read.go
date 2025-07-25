package set2

import (
	"bytes"
	"errors"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	pcommon "github.com/pendulea/pendule-common"
	log "github.com/sirupsen/logrus"
)

func (state *AssetState) NewTX(update bool) *badger.Txn {
	return state.SetRef.db.NewTransaction(update)
}

// Get a list of ticks including t0 and excluding t1 (t0 <= Time(tick) < t1)
func (state *AssetState) GetInDataRange(t0, t1 pcommon.TimeUnit, timeframe time.Duration, txn *badger.Txn, iter *badger.Iterator, recordReading bool) (pcommon.DataList, error) {
	if t1 < t0 {
		return nil, errors.New("t1 must be after t0")
	}

	label, err := pcommon.Format.TimeFrameToLabel(timeframe)
	if err != nil {
		return nil, err
	}

	// Open a read-only BadgerDB transaction
	if txn == nil {
		txn = state.SetRef.db.NewTransaction(false)
		defer txn.Discard()
	}

	startKey := state.GetDataKey(label, t0)
	limitKey := state.GetDataKey(label, t1)

	if iter == nil {
		opts := badger.DefaultIteratorOptions
		opts.Reverse = false
		opts.PrefetchValues = true
		iter = txn.NewIterator(opts)
		defer iter.Close()
	}

	ret := pcommon.NewTypeTimeArray(state.DataType())

	// Iterate over the keys and retrieve values within the range
	for iter.Seek(startKey); iter.Valid(); iter.Next() {
		key := iter.Item().Key()
		if bytes.Compare(key, limitKey) >= 0 {
			break
		}

		_, dataTime, err := state.ParseDataKey(key)
		if err != nil {
			return nil, err
		}

		value, err := iter.Item().ValueCopy(nil)
		if err != nil {
			return nil, err
		}

		unraw, err := pcommon.ParseTypeData(state.DataType(), value, dataTime)
		if err != nil {
			return nil, err
		}
		ret = ret.Append(unraw)
	}

	if recordReading {
		go func() {
			err := state.onNewRead(timeframe)
			if err != nil {
				log.WithFields(log.Fields{
					"symbol": state.SetRef.ID(),
					"error":  err.Error(),
				}).Error("Error setting last read")
			}
		}()
	}

	return ret, nil
}

type DataLimitSettings struct {
	TimeFrame      time.Duration
	Limit          int
	OffsetUnixTime pcommon.TimeUnit
	StartByEnd     bool
}

func (state *AssetState) GetDataLimit(settings DataLimitSettings, recordReading bool) (pcommon.DataList, error) {
	timeFrame := settings.TimeFrame
	limit := settings.Limit
	offsetUnixTime := settings.OffsetUnixTime
	startByEnd := settings.StartByEnd

	ret := pcommon.NewTypeTimeArray(state.DataType())

	if limit > 1 && !state.IsTimeframeSupported(timeFrame) {
		return nil, nil
	}

	label, err := pcommon.Format.TimeFrameToLabel(timeFrame)
	if err != nil {
		return nil, err
	}

	// Open a read-only BadgerDB transaction
	txn := state.SetRef.db.NewTransaction(false)
	defer txn.Discard()

	var limitTime pcommon.TimeUnit
	if startByEnd {
		limitTime = state.DataHistoryTime0()
	} else {
		lastData, rowTime, err := state.GetLatestData(timeFrame)
		if err != nil || lastData == nil {
			return nil, err
		}
		limitTime = rowTime
	}

	startKey := state.GetDataKey(label, offsetUnixTime)
	limitKey := state.GetDataKey(label, limitTime)

	opts := badger.DefaultIteratorOptions
	opts.PrefetchValues = true
	opts.Reverse = startByEnd
	if limit > 0 && limit < 100 {
		opts.PrefetchSize = limit
	}

	iter := txn.NewIterator(opts)
	defer iter.Close()

	count := 0

	// Iterate over the keys and retrieve values within the range
	for iter.Seek(startKey); iter.Valid(); iter.Next() {
		key := iter.Item().Key()
		if bytes.Equal(key, startKey) {
			continue
		}
		if (startByEnd && bytes.Compare(key, limitKey) < 0) || (!startByEnd && bytes.Compare(key, limitKey) > 0) {
			break
		}

		_, rowTime, err := state.ParseDataKey(key)
		if err != nil {
			return nil, err
		}
		value, err := iter.Item().ValueCopy(nil)
		if err != nil {
			return nil, err
		}

		unraw, err := pcommon.ParseTypeData(state.DataType(), value, rowTime)
		if err != nil {
			return nil, err
		}

		if !startByEnd {
			ret = ret.Append(unraw)
		} else {
			ret = ret.Prepend(unraw)
		}

		count += 1
		if count == limit {
			break
		}
	}

	if recordReading {
		go func() {
			err := state.onNewRead(timeFrame)
			if err != nil {
				log.WithFields(log.Fields{
					"symbol": state.SetRef.ID(),
					"error":  err.Error(),
				}).Error("Error setting last read")
			}
		}()
	}

	return ret, nil
}

func (state *AssetState) getSingleData(settings DataLimitSettings) (pcommon.Data, pcommon.TimeUnit, error) {
	settings.Limit = 1
	list, err := state.GetDataLimit(settings, false)
	if err != nil {
		return nil, 0, err
	}
	if list.Len() == 0 {
		return nil, 0, nil
	}

	first := list.First()
	return first, first.GetTime(), nil
}

func (state *AssetState) GetEarliestData(timeframe time.Duration) (interface{}, pcommon.TimeUnit, error) {
	settings := DataLimitSettings{
		TimeFrame:      timeframe,
		Limit:          1,
		OffsetUnixTime: 0,
		StartByEnd:     false,
	}
	return state.getSingleData(settings)
}

func (state *AssetState) GetLatestData(timeframe time.Duration) (interface{}, pcommon.TimeUnit, error) {
	settings := DataLimitSettings{
		TimeFrame:      timeframe,
		Limit:          1,
		OffsetUnixTime: pcommon.NewTimeUnitFromTime(time.Now()),
		StartByEnd:     true,
	}
	return state.getSingleData(settings)
}

func (state *AssetState) __debug__printEntireDataSet() {
	// Open a read-only transaction
	txn := state.SetRef.db.NewTransaction(false)
	defer txn.Discard() // Ensure the transaction is discarded after use

	// Set up an iterator with a prefix
	opts := badger.DefaultIteratorOptions
	opts.Prefix = state.GetAssetKey()
	it := txn.NewIterator(opts)
	defer it.Close() // Ensure the iterator is closed after use

	// Iterate over keys with the specified prefix
	for it.Seek(opts.Prefix); it.ValidForPrefix(opts.Prefix); it.Next() {
		item := it.Item()
		key := item.Key()
		value, err := item.ValueCopy(nil)
		if err != nil {
			log.Printf("Error reading value: %s\n", err)
			return
		}
		// Do something with the key or the value
		log.Printf("%s : %x\n", string(key), value)
		// You can also fetch and process the value if needed
	}

}
