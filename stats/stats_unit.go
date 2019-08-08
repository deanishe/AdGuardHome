package stats

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/AdguardTeam/golibs/log"
	bolt "github.com/etcd-io/bbolt"
)

const (
	maxDomains = 100 // max number of top domains to store in file or return via Get()
	maxClients = 100 // max number of top clients to store in file or return via Get()
)

// statsCtx - global context
type statsCtx struct {
	limit    int // in hours
	filename string
	unitID   unitIDCallback
	db       *bolt.DB

	unit     *unit
	unitLock sync.Mutex
}

// data for 1 time unit
type unit struct {
	id int

	nTotal  int
	nResult []int
	timeSum int // usec

	// top:
	domains        map[string]int
	blockedDomains map[string]int
	clients        map[string]int
}

// name-count pair
type countPair struct {
	Name  string
	Count uint
}

// structure for storing data in file
type unitDB struct {
	NTotal  uint
	NResult []uint

	Domains        []countPair
	BlockedDomains []countPair
	Clients        []countPair

	TimeAvg uint // usec
}

func createObject(filename string, limit int, unitID unitIDCallback) *statsCtx {
	s := statsCtx{}
	s.limit = limit * 24
	s.filename = filename
	s.unitID = newUnitID
	if unitID != nil {
		s.unitID = unitID
	}

	if !s.dbOpen() {
		return nil
	}

	id := s.unitID()
	tx := s.beginTxn(true)
	var udb *unitDB
	if tx != nil {
		log.Tracef("Deleting old units...")
		firstID := id - s.limit - 1
		unitDel := 0
		forEachBkt := func(name []byte, b *bolt.Bucket) error {
			id := btoi(name)
			if id < firstID {
				tx.DeleteBucket(name)
				log.Debug("Stats: deleted unit %d", id)
				unitDel++
				return nil
			}
			return fmt.Errorf("")
		}
		_ = tx.ForEach(forEachBkt)

		udb = s.loadUnitFromDB(tx, id)

		if unitDel != 0 {
			tx.Commit()
			log.Tracef("tx.Commit")
		} else {
			tx.Rollback()
		}
	}

	u := unit{}
	s.initUnit(&u, id)
	if udb != nil {
		deserialize(&u, udb)
	}
	s.unit = &u

	go s.periodicFlush()

	log.Debug("Stats: initialized")
	return &s
}

func (s *statsCtx) dbOpen() bool {
	var err error
	log.Tracef("db.Open...")
	s.db, err = bolt.Open(s.filename, 0644, nil)
	if err != nil {
		log.Error("Stats: open DB: %s: %s", s.filename, err)
		return false
	}
	log.Tracef("db.Open")
	return true
}

// Atomically swap the currently active unit with a new value
// Return old value
func (s *statsCtx) swapUnit(new *unit) *unit {
	s.unitLock.Lock()
	u := s.unit
	s.unit = new
	s.unitLock.Unlock()
	return u
}

// Get unit ID for the current hour
func newUnitID() int {
	return int(time.Now().Unix() / (60 * 60))
}

// Initialize a unit
func (s *statsCtx) initUnit(u *unit, id int) {
	u.id = id
	u.nResult = make([]int, rLast)
	u.domains = make(map[string]int)
	u.blockedDomains = make(map[string]int)
	u.clients = make(map[string]int)
}

// Open a DB transaction
func (s *statsCtx) beginTxn(wr bool) *bolt.Tx {
	db := s.db
	if db == nil {
		return nil
	}

	log.Tracef("db.Begin...")
	tx, err := db.Begin(wr)
	if err != nil {
		log.Error("db.Begin: %s", err)
		return nil
	}
	log.Tracef("db.Begin")
	return tx
}

// Get unit name
func unitName(id int) []byte {
	return itob(id)
}

// Convert integer to 8-byte array (big endian)
func itob(v int) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(v))
	return b
}

// Convert 8-byte array (big endian) to integer
func btoi(b []byte) int {
	return int(binary.BigEndian.Uint64(b))
}

// Flush the current unit to DB and delete an old unit when a new hour is started
func (s *statsCtx) periodicFlush() {
	for s.unit != nil {
		id := s.unitID()
		if s.unit.id == id {
			time.Sleep(time.Second)
			continue
		}

		nu := unit{}
		s.initUnit(&nu, id)
		u := s.swapUnit(&nu)
		udb := serialize(u)

		tx := s.beginTxn(true)
		if tx == nil {
			continue
		}
		ok1 := s.flushUnitToDB(tx, u.id, udb)
		ok2 := s.deleteUnit(tx, id-s.limit)
		if ok1 || ok2 {
			tx.Commit()
			log.Tracef("tx.Commit")
		} else {
			tx.Rollback()
		}
	}
	log.Tracef("periodicFlush() exited")
}

// Delete unit's data from file
func (s *statsCtx) deleteUnit(tx *bolt.Tx, id int) bool {
	err := tx.DeleteBucket(unitName(id))
	if err != nil {
		log.Tracef("bolt DeleteBucket: %s", err)
		return false
	}
	log.Debug("Stats: deleted unit %d", id)
	return true
}

func convertMapToArray(m map[string]int, max int) []countPair {
	a := []countPair{}
	for k, v := range m {
		pair := countPair{}
		pair.Name = k
		pair.Count = uint(v)
		a = append(a, pair)
	}
	less := func(i, j int) bool {
		if a[i].Count >= a[j].Count {
			return true
		}
		return false
	}
	sort.Slice(a, less)
	if max > len(a) {
		max = len(a)
	}
	return a[:max]
}

func convertArrayToMap(a []countPair) map[string]int {
	m := map[string]int{}
	for _, it := range a {
		m[it.Name] = int(it.Count)
	}
	return m
}

func serialize(u *unit) *unitDB {
	udb := unitDB{}
	udb.NTotal = uint(u.nTotal)
	for _, it := range u.nResult {
		udb.NResult = append(udb.NResult, uint(it))
	}
	if u.nTotal != 0 {
		udb.TimeAvg = uint(u.timeSum / u.nTotal)
	}
	udb.Domains = convertMapToArray(u.domains, maxDomains)
	udb.BlockedDomains = convertMapToArray(u.blockedDomains, maxDomains)
	udb.Clients = convertMapToArray(u.clients, maxClients)
	return &udb
}

func deserialize(u *unit, udb *unitDB) {
	u.nTotal = int(udb.NTotal)
	for _, it := range udb.NResult {
		u.nResult = append(u.nResult, int(it))
	}
	u.domains = convertArrayToMap(udb.Domains)
	u.blockedDomains = convertArrayToMap(udb.BlockedDomains)
	u.clients = convertArrayToMap(udb.Clients)
	u.timeSum = int(udb.TimeAvg) * u.nTotal
}

func (s *statsCtx) flushUnitToDB(tx *bolt.Tx, id int, udb *unitDB) bool {
	log.Tracef("Flushing unit %d", id)

	bkt, err := tx.CreateBucketIfNotExists(unitName(id))
	if err != nil {
		log.Error("tx.CreateBucketIfNotExists: %s", err)
		return false
	}

	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	err = enc.Encode(udb)
	if err != nil {
		log.Error("gob.Encode: %s", err)
		return false
	}

	err = bkt.Put([]byte{0}, buf.Bytes())
	if err != nil {
		log.Error("bkt.Put: %s", err)
		return false
	}

	return true
}

func (s *statsCtx) loadUnitFromDB(tx *bolt.Tx, id int) *unitDB {
	bkt := tx.Bucket(unitName(id))
	if bkt == nil {
		return nil
	}

	log.Tracef("Loading unit %d", id)

	var buf bytes.Buffer
	buf.Write(bkt.Get([]byte{0}))
	dec := gob.NewDecoder(&buf)
	udb := unitDB{}
	err := dec.Decode(&udb)
	if err != nil {
		log.Error("gob Decode: %s", err)
		return nil
	}

	return &udb
}

func convertTopArray(a []countPair) []map[string]uint {
	m := []map[string]uint{}
	for _, it := range a {
		ent := map[string]uint{}
		ent[it.Name] = it.Count
		m = append(m, ent)
	}
	return m
}

func (s *statsCtx) Configurate(limit int) {
	if limit < 0 {
		return
	}
	s.limit = limit * 24
	log.Debug("Stats: set limit: %d", limit)
}

func (s *statsCtx) Close() {
	u := s.swapUnit(nil)
	udb := serialize(u)
	tx := s.beginTxn(true)
	if tx != nil {
		if s.flushUnitToDB(tx, u.id, udb) {
			tx.Commit()
			log.Tracef("tx.Commit")
		} else {
			tx.Rollback()
		}
	}

	if s.db != nil {
		log.Tracef("db.Close...")
		s.db.Close()
		log.Tracef("db.Close")
	}

	log.Debug("Stats: closed")
}

func (s *statsCtx) Clear() {
	tx := s.beginTxn(true)
	if tx != nil {
		db := s.db
		s.db = nil
		tx.Rollback()

		db.Close()
		log.Tracef("db.Close")
		s.dbOpen()
	}

	u := unit{}
	s.initUnit(&u, s.unitID())
	s.swapUnit(&u)

	log.Debug("Stats: cleared")
}

func (s *statsCtx) Update(e Entry) {
	if e.Result == 0 ||
		len(e.Domain) == 0 ||
		!(len(e.Client) == 4 || len(e.Client) == 16) {
		return
	}
	client := e.Client.String()

	s.unitLock.Lock()
	u := s.unit

	u.nResult[e.Result]++

	if e.Result == RNotFiltered {
		u.domains[e.Domain]++
	} else {
		u.blockedDomains[e.Domain]++
	}

	u.clients[client]++
	u.timeSum += int(e.Time)
	u.nTotal++
	s.unitLock.Unlock()
}

func (s *statsCtx) GetData(timeUnit TimeUnit) map[string]interface{} {
	d := map[string]interface{}{}

	tx := s.beginTxn(false)
	if tx == nil {
		return nil
	}

	units := []*unitDB{} //per-hour units
	lastID := s.unitID()
	firstID := lastID - s.limit + 1
	for i := firstID; i != lastID; i++ {
		u := s.loadUnitFromDB(tx, i)
		if u == nil {
			u = &unitDB{}
			u.NResult = make([]uint, rLast)
		}
		units = append(units, u)
	}

	tx.Rollback()

	s.unitLock.Lock()
	cu := serialize(s.unit)
	cuID := s.unit.id
	s.unitLock.Unlock()
	if cuID != lastID {
		units = units[1:]
	}
	units = append(units, cu)

	if len(units) != s.limit {
		log.Fatalf("len(units) != s.limit: %d %d", len(units), s.limit)
	}

	// per time unit counters:

	a := []uint{}
	if timeUnit == Hours {
		for _, u := range units {
			a = append(a, u.NTotal)
		}
	} else {
		var sum uint
		id := firstID
		for _, u := range units {
			sum += u.NTotal
			if (id % 24) == 0 {
				a = append(a, sum)
				sum = 0
			}
			id++
		}
	}
	d["dns_queries"] = a

	a = []uint{}
	if timeUnit == Hours {
		for _, u := range units {
			a = append(a, u.NResult[RFiltered])
		}
	} else {
		var sum uint
		id := firstID
		for _, u := range units {
			sum += u.NResult[RFiltered]
			if (id % 24) == 0 {
				a = append(a, sum)
				sum = 0
			}
			id++
		}
	}
	d["blocked_filtering"] = a

	a = []uint{}
	if timeUnit == Hours {
		for _, u := range units {
			a = append(a, u.NResult[RSafeBrowsing])
		}
	} else {
		var sum uint
		id := firstID
		for _, u := range units {
			sum += u.NResult[RSafeBrowsing]
			if (id % 24) == 0 {
				a = append(a, sum)
				sum = 0
			}
			id++
		}
	}
	d["replaced_safebrowsing"] = a

	a = []uint{}
	if timeUnit == Hours {
		for _, u := range units {
			a = append(a, u.NResult[RParental])
		}
	} else {
		var sum uint
		id := firstID
		for _, u := range units {
			sum += u.NResult[RParental]
			if (id % 24) == 0 {
				a = append(a, sum)
				sum = 0
			}
			id++
		}
	}
	d["replaced_parental"] = a

	// top counters:

	m := map[string]int{}
	for _, u := range units {
		for _, it := range u.Domains {
			m[it.Name] = int(it.Count)
		}
	}
	a2 := convertMapToArray(m, maxDomains)
	d["top_queried_domains"] = convertTopArray(a2)

	m = map[string]int{}
	for _, u := range units {
		for _, it := range u.BlockedDomains {
			m[it.Name] = int(it.Count)
		}
	}
	a2 = convertMapToArray(m, maxDomains)
	d["top_blocked_domains"] = convertTopArray(a2)

	m = map[string]int{}
	for _, u := range units {
		for _, it := range u.Clients {
			m[it.Name] = int(it.Count)
		}
	}
	a2 = convertMapToArray(m, maxClients)
	d["top_clients"] = convertTopArray(a2)

	// total counters:

	sum := unitDB{}
	timeN := 0
	sum.NResult = make([]uint, rLast)
	for _, u := range units {
		sum.NTotal += u.NTotal
		sum.TimeAvg += u.TimeAvg
		if u.TimeAvg != 0 {
			timeN++
		}
		sum.NResult[RFiltered] += u.NResult[RFiltered]
		sum.NResult[RSafeBrowsing] += u.NResult[RSafeBrowsing]
		sum.NResult[RSafeSearch] += u.NResult[RSafeSearch]
		sum.NResult[RParental] += u.NResult[RParental]
	}

	d["num_dns_queries"] = sum.NTotal
	d["num_blocked_filtering"] = sum.NResult[RFiltered]
	d["num_replaced_safebrowsing"] = sum.NResult[RSafeBrowsing]
	d["num_replaced_safesearch"] = sum.NResult[RSafeSearch]
	d["num_replaced_parental"] = sum.NResult[RParental]

	d["avg_processing_time"] = float64(sum.TimeAvg/uint(timeN)) / 1000000

	d["time_units"] = "hours"
	if timeUnit == Days {
		d["time_units"] = "days"
	}

	return d
}
