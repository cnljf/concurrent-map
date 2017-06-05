package cmap

import (
	"encoding/json"
	"sync"
)

// A "thread" safe map of type string:Anything.
// To avoid lock bottlenecks this map is dived to several map shards.
type ConcurrentMap []*ConcurrentMapShared

// A "thread" safe string to anything map.
type ConcurrentMapShared struct {
	items        map[string]interface{}
	sync.RWMutex // Read Write mutex, guards access to internal map.
}

// Creates a new concurrent map.
func New(shardCount int) ConcurrentMap {
	m := make(ConcurrentMap, shardCount)
	for i := 0; i < shardCount; i++ {
		m[i] = &ConcurrentMapShared{items: make(map[string]interface{})}
	}
	return m
}

// Returns shard under given key
func (m ConcurrentMap) GetShard(key string) *ConcurrentMapShared {
	return m[uint(fnv32(key))%uint(len(m))]
}

func (m ConcurrentMap) MSet(data map[string]interface{}) {
	for key, value := range data {
		shard := m.GetShard(key)
		shard.Lock()
		shard.items[key] = value
		shard.Unlock()
	}
}

// Sets the given value under the specified key.
func (m ConcurrentMap) Set(key string, value interface{}) {
	// Get map shard.
	shard := m.GetShard(key)
	shard.Lock()
	shard.items[key] = value
	shard.Unlock()
}

// Callback to return new element to be inserted into the map
// It is called while lock is held, therefore it MUST NOT
// try to access other keys in same map, as it can lead to deadlock since
// Go sync.RWLock is not reentrant
type UpsertCb func(exist bool, valueInMap interface{}, newValue interface{}) interface{}

// Insert or Update - updates existing element or inserts a new one using UpsertCb
func (m ConcurrentMap) Upsert(key string, value interface{}, cb UpsertCb) (res interface{}) {
	shard := m.GetShard(key)
	shard.Lock()
	v, ok := shard.items[key]
	res = cb(ok, v, value)
	shard.items[key] = res
	shard.Unlock()
	return res
}

// Sets the given value under the specified key if no value was associated with it.
func (m ConcurrentMap) SetIfAbsent(key string, value interface{}) bool {
	// Get map shard.
	shard := m.GetShard(key)
	shard.Lock()
	_, ok := shard.items[key]
	if !ok {
		shard.items[key] = value
	}
	shard.Unlock()
	return !ok
}

// Retrieves an element from map under given key.
func (m ConcurrentMap) Get(key string) (interface{}, bool) {
	// Get shard
	shard := m.GetShard(key)
	shard.RLock()
	// Get item from shard.
	val, ok := shard.items[key]
	shard.RUnlock()
	return val, ok
}

// Returns the number of elements within the map.
func (m ConcurrentMap) Count() int {
	count := 0
	for i := 0; i < len(m); i++ {
		shard := m[i]
		shard.RLock()
		count += len(shard.items)
		shard.RUnlock()
	}
	return count
}

// Looks up an item under specified key
func (m ConcurrentMap) Has(key string) bool {
	// Get shard
	shard := m.GetShard(key)
	shard.RLock()
	// See if element is within shard.
	_, ok := shard.items[key]
	shard.RUnlock()
	return ok
}

// Removes an element from the map.
func (m ConcurrentMap) Remove(key string) {
	// Try to get shard.
	shard := m.GetShard(key)
	shard.Lock()
	delete(shard.items, key)
	shard.Unlock()
}

// Removes an element from the map and returns it
func (m ConcurrentMap) Pop(key string) (v interface{}, exists bool) {
	// Try to get shard.
	shard := m.GetShard(key)
	shard.Lock()
	v, exists = shard.items[key]
	delete(shard.items, key)
	shard.Unlock()
	return v, exists
}

// Checks if map is empty.
func (m ConcurrentMap) IsEmpty() bool {
	return m.Count() == 0
}

// Used by the Iter & IterBuffered functions to wrap two variables together over a channel,
type Tuple struct {
	Key string
	Val interface{}
}

// Returns an iterator which could be used in a for range loop.
//
// Deprecated: using IterBuffered() will get a better performence
func (m ConcurrentMap) Iter() <-chan Tuple {
	return m.iter(false)
}

func (m ConcurrentMap) iter(onlyKey bool) <-chan Tuple {
	ch := make(chan Tuple, 100)
	go func() {
		defer close(ch)

		chans := make([]chan Tuple, len(m))
		chansLength := make([]int, len(m))
		var totalLength = 0
		for index, shard := range m {
			shard.RLock()
			if len(shard.items) > totalLength {
				chansLength[index] = len(shard.items) - totalLength
				chans[index] = make(chan Tuple, chansLength[index])
				totalLength = len(shard.items)
			}

			var pos int
			var targetChan int = -1
			var currentLength int
			for key, val := range shard.items {
				if currentLength <= pos {
					for i := targetChan + 1; i < len(chansLength); i++ {
						if chansLength[i] > 0 {
							currentLength += chansLength[i]
							targetChan = i
							break
						}
					}
				}

				if onlyKey {
					chans[targetChan] <- Tuple{Key: key}
				} else {
					chans[targetChan] <- Tuple{Key: key, Val: val}
				}
				pos++
			}
			shard.RUnlock()

			currentLength = 0
			var t Tuple
			for i, length := range chansLength {
				if length <= 0 {
					continue
				}
				if currentLength+length < pos {
					for j := 0; j < length; j++ {
						t = <-chans[i]
						ch <- t
					}
					currentLength += length
				} else {
					for j := 0; j < (pos - currentLength); j++ {
						t = <-chans[i]
						ch <- t
					}
					break
				}
			}
		}
	}()

	return ch
}

func (m ConcurrentMap) IterKeys() <-chan string {
	ch := make(chan string, 100)
	go func() {
		defer close(ch)
		for t := range m.iter(true) {
			ch <- t.Key
		}
	}()

	return ch
}

// Returns a buffered iterator which could be used in a for range loop.
func (m ConcurrentMap) IterBuffered() <-chan Tuple {
	ch := make(chan Tuple, 100)
	go func() {
		chans := snapshot(m)
		fanIn(chans, ch)
	}()

	return ch
}

// Returns a array of channels that contains elements in each shard,
// which likely takes a snapshot of `m`.
// It returns once the size of each buffered channel is determined,
// before all the channels are populated using goroutines.
func snapshot(m ConcurrentMap) (chans []chan Tuple) {
	chans = make([]chan Tuple, len(m))
	wg := sync.WaitGroup{}
	wg.Add(len(m))
	// Foreach shard.
	for index, shard := range m {
		go func(index int, shard *ConcurrentMapShared) {
			// Foreach key, value pair.
			shard.RLock()
			chans[index] = make(chan Tuple, len(shard.items))
			wg.Done()
			for key, val := range shard.items {
				chans[index] <- Tuple{key, val}
			}
			shard.RUnlock()
			close(chans[index])
		}(index, shard)
	}
	wg.Wait()
	return chans
}

// fanIn reads elements from channels `chans` into channel `out`
func fanIn(chans []chan Tuple, out chan Tuple) {
	wg := sync.WaitGroup{}
	wg.Add(len(chans))
	for _, ch := range chans {
		go func(ch chan Tuple) {
			for t := range ch {
				out <- t
			}
			wg.Done()
		}(ch)
	}
	wg.Wait()
	close(out)
}

// Returns all items as map[string]interface{}
func (m ConcurrentMap) Items() map[string]interface{} {
	tmp := make(map[string]interface{})

	// Insert items to temporary map.
	for item := range m.Iter() {
		tmp[item.Key] = item.Val
	}

	return tmp
}

// Iterator callback,called for every key,value found in
// maps. RLock is held for all calls for a given shard
// therefore callback sess consistent view of a shard,
// but not across the shards
type IterCb func(key string, v interface{})

// Callback based iterator, cheapest way to read
// all elements in a map.
func (m ConcurrentMap) IterCb(fn IterCb) {
	for idx := range m {
		shard := (m)[idx]
		shard.RLock()
		for key, value := range shard.items {
			fn(key, value)
		}
		shard.RUnlock()
	}
}

// Return all keys as []string
func (m ConcurrentMap) Keys() []string {
	count := m.Count()
	ch := make(chan string, count)
	go func() {
		// Foreach shard.
		wg := sync.WaitGroup{}
		wg.Add(len(m))
		for _, shard := range m {
			go func(shard *ConcurrentMapShared) {
				// Foreach key, value pair.
				shard.RLock()
				for key := range shard.items {
					ch <- key
				}
				shard.RUnlock()
				wg.Done()
			}(shard)
		}
		wg.Wait()
		close(ch)
	}()

	// Generate keys
	keys := make([]string, 0, count)
	for k := range ch {
		keys = append(keys, k)
	}
	return keys
}

//Reviles ConcurrentMap "private" variables to json marshal.
func (m ConcurrentMap) MarshalJSON() ([]byte, error) {
	// Create a temporary map, which will hold all item spread across shards.
	tmp := make(map[string]interface{})

	// Insert items to temporary map.
	for item := range m.Iter() {
		tmp[item.Key] = item.Val
	}
	return json.Marshal(tmp)
}

func fnv32(key string) uint32 {
	hash := uint32(2166136261)
	const prime32 = uint32(16777619)
	for i := 0; i < len(key); i++ {
		hash *= prime32
		hash ^= uint32(key[i])
	}
	return hash
}

// Concurrent map uses Interface{} as its value, therefor JSON Unmarshal
// will probably won't know which to type to unmarshal into, in such case
// we'll end up with a value of type map[string]interface{}, In most cases this isn't
// out value type, this is why we've decided to remove this functionality.

// func (m *ConcurrentMap) UnmarshalJSON(b []byte) (err error) {
// 	// Reverse process of Marshal.

// 	tmp := make(map[string]interface{})

// 	// Unmarshal into a single map.
// 	if err := json.Unmarshal(b, &tmp); err != nil {
// 		return nil
// 	}

// 	// foreach key,value pair in temporary map insert into our concurrent map.
// 	for key, val := range tmp {
// 		m.Set(key, val)
// 	}
// 	return nil
// }
