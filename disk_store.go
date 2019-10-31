package mhist

import (
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"github.com/alexmorten/mhist/models"
)

const maxBuffer = 12 * 1024
const timeBetweenWrites = 20 * time.Second

var dataPath = "data"

//DiskStore handles buffered writes to and reads from Disk
type DiskStore struct {
	*DiskWriter

	meta     *DiskMeta
	addChan  chan addMessage
	readChan chan readMessage
	stopChan chan struct{}
}

type addMessage struct {
	name        string
	measurement SerializedMeasurement
	rawValue    []byte
}

type readResult map[string][]models.Measurement

type readMessage struct {
	fromTs           int64
	toTs             int64
	filterDefinition models.FilterDefinition
	resultChan       chan readResult
}

//NewDiskStore initializes the DiskBlockRoutine
func NewDiskStore(maxFileSize, maxDiskSize int) (*DiskStore, error) {
	err := os.MkdirAll(dataPath, os.ModePerm)
	if err != nil {
		return nil, err
	}

	writer, err := NewDiskWriter(maxFileSize, maxDiskSize)
	if err != nil {
		return nil, err
	}

	store := &DiskStore{
		meta:       InitMetaFromDisk(),
		DiskWriter: writer,
		addChan:    make(chan addMessage),
		readChan:   make(chan readMessage),
		stopChan:   make(chan struct{}),
	}

	go store.Listen()
	return store, nil
}

//Notify DiskStore about new Measurement
func (s *DiskStore) Notify(name string, m models.Measurement) {
	s.Add(name, m)
}

//Add measurement to block
func (s *DiskStore) Add(name string, measurement models.Measurement) {
	id, err := s.meta.GetOrCreateID(name, measurement.Type())
	if err != nil {
		//measurement is probably of different type than it used to be, just ignore for now
		// should probably trigger some kind of cleanup
		return
	}

	var valueOrValueID float64
	var rawValue []byte

	switch measurement.(type) {
	case *models.Numerical:
		valueOrValueID = measurement.(*models.Numerical).Value
	case *models.Categorical:
		valueOrValueID = s.meta.GetValueIDForCategoricalValue(id, measurement.(*models.Categorical).Value)
	case *models.Raw:
		rawValue = measurement.(*models.Raw).Value
	}

	s.addChan <- addMessage{
		name:        name,
		measurement: SerializedMeasurement{ID: id, Ts: measurement.Timestamp(), Value: valueOrValueID},
		rawValue:    rawValue,
	}
}

//GetMeasurementsInTimeRange for all measurement names
func (s *DiskStore) GetMeasurementsInTimeRange(start, end int64, filterDefiniton models.FilterDefinition) map[string][]models.Measurement {
	resultChan := make(chan readResult)
	s.readChan <- readMessage{
		fromTs:           start,
		toTs:             end,
		filterDefinition: filterDefiniton,
		resultChan:       resultChan,
	}
	return <-resultChan
}

//GetAllStoredInfos from meta
func (s *DiskStore) GetAllStoredInfos() []MeasurementTypeInfo {
	return s.meta.GetAllStoredInfos()
}

//Shutdown DiskBlock goroutine
func (s *DiskStore) Shutdown() {
	s.stopChan <- struct{}{}
}

//Listen for new measurements
func (s *DiskStore) Listen() {
	timer := time.NewTimer(timeBetweenWrites)
loop:
	for {
		select {
		case <-s.stopChan:
			s.commit()
			break loop
		case <-timer.C:
			s.commit()
			timer.Stop()
			timer.Reset(timeBetweenWrites)
		case message := <-s.readChan:
			message.resultChan <- s.handleRead(message.fromTs, message.toTs, message.filterDefinition)
		case message := <-s.addChan:
			s.handleAdd(message)
		}
	}
}

//Commit the buffered writes to actual disk
func (s *DiskStore) commit() {
	if s.block.Size() == 0 {
		return
	}

	fileList, err := GetSortedFileList()
	if err != nil {
		log.Printf("couldn't get file List: %v", err)
		return
	}
	defer func() { s.block = s.block[:0] }()
	if len(fileList) == 0 {
		WriteBlockToFile(s.block)
		return
	}
	latestFile := fileList[len(fileList)-1]
	if latestFile.size < s.maxFileSize {
		AppendBlockToFile(latestFile, s.block)
		return
	}
	WriteBlockToFile(s.block)

	if fileList.TotalSize() > s.maxDiskSize {
		oldestFile := fileList[0]
		os.Remove(filepath.Join(dataPath, oldestFile.name))
	}
}

func (s *DiskStore) handleAdd(m addMessage) {
	s.block = append(s.block, m.measurement)

	if s.block.Size() > maxBuffer {
		s.commit()
	}
}

func (s *DiskStore) handleRead(start, end int64, filterDefinition models.FilterDefinition) readResult {
	result := readResult{}
	files, err := GetFilesInTimeRange(start, end)
	if err != nil {
		log.Println(err)
		return readResult{}
	}
	filter := models.NewFilterCollection(filterDefinition)
	for _, file := range files {
		byteSlice, err := ioutil.ReadFile(filepath.Join(dataPath, file.name))
		if err != nil {
			log.Println(err)
			continue
		}
		block := BlockFromByteSlice(byteSlice)

		s.appendPassingMeasurements(block, start, end, filter, result)

	}

	// the range we read includes the currently buffered block
	if len(files) == 0 || end > files[len(files)-1].latestTs {
		s.appendPassingMeasurements(s.block, start, end, filter, result)
	}

	return result
}

func (s *DiskStore) appendPassingMeasurements(block Block, start, end int64, filter *models.FilterCollection, result readResult) {
	for _, serializedMeasurement := range block {
		name := s.meta.GetNameForID(serializedMeasurement.ID)
		if name == "" {
			continue
		}

		if serializedMeasurement.Ts < start || serializedMeasurement.Ts > end {
			continue
		}

		measurementType := s.meta.GetTypeForID(serializedMeasurement.ID)
		if measurementType == 0 {
			continue
		}

		var measurement models.Measurement
		switch measurementType {
		case models.MeasurementNumerical:
			measurement = &models.Numerical{Ts: serializedMeasurement.Ts, Value: serializedMeasurement.Value}

		case models.MeasurementCategorical:
			measurement = &models.Categorical{
				Ts:    serializedMeasurement.Ts,
				Value: s.meta.CategoricalMapping.GetOrCreateValueIDMap(serializedMeasurement.ID).ValueIDToValue[serializedMeasurement.Value],
			}
		}
		if filter.Passes(name, measurement) {
			result[name] = append(result[name], measurement)
		}
	}
}

//SerializedMeasurement is a numerical measureent extended by ID, can be dumped to disk directly
type SerializedMeasurement struct {
	ID    int64
	Ts    int64
	Value float64
	Size  int64
}

var serializedMeasurementSize = int(unsafe.Sizeof(SerializedMeasurement{}))
