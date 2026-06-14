// Package racy demonstrates multiple classic Go race conditions.
// This file is intentionally racy — it exists to be analyzed by racevis.
package racy

import (
	"sync"
	"time"
)

// ----- Race 1: Write/Write on shared counter -----

type UnsafeCounter struct {
	value int
}

func (c *UnsafeCounter) Increment() {
	c.value++ // read-modify-write, not atomic
}

func (c *UnsafeCounter) Value() int {
	return c.value
}

func RunCounterRace() int {
	c := &UnsafeCounter{}
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.Increment()
			}
		}()
	}
	wg.Wait()
	return c.Value()
}

// ----- Race 2: Closure capturing loop variable -----

func RunClosureRace() []int {
	results := make([]int, 5)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { // captures i by reference — classic race
			defer wg.Done()
			results[i%5] = i * 2 // i is shared, not captured by value
		}()
	}
	wg.Wait()
	return results
}

// ----- Race 3: Map concurrent read/write -----

func RunMapRace() map[string]int {
	m := make(map[string]int)
	var wg sync.WaitGroup

	// writer goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			m["key"] = i
			time.Sleep(time.Microsecond)
		}
	}()

	// reader goroutine — concurrent with writer
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_ = m["key"]
			time.Sleep(time.Microsecond)
		}
	}()

	wg.Wait()
	return m
}

// ----- Race 4: Check-then-act (init race) -----

var (
	instance *singleton
	once     sync.Mutex // intentionally wrong — should be sync.Once
)

type singleton struct {
	data string
}

func getInstance() *singleton {
	if instance == nil { // check
		once.Lock()
		if instance == nil { // double-check, but still racy without sync.Once
			instance = &singleton{data: "initialized"}
		}
		once.Unlock()
	}
	return instance // act — another goroutine may be mid-write
}

func RunInitRace() string {
	var wg sync.WaitGroup
	results := make([]string, 10)
	for i := 0; i < 10; i++ {
		idx := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := getInstance()
			results[idx] = s.data
		}()
	}
	wg.Wait()
	return results[0]
}
