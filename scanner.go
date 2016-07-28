// Copyright (C) 2016 Opsmate, Inc.
//
// This Source Code Form is subject to the terms of the Mozilla
// Public License, v. 2.0. If a copy of the MPL was not distributed
// with this file, You can obtain one at http://mozilla.org/MPL/2.0/.
//
// This software is distributed WITHOUT A WARRANTY OF ANY KIND.
// See the Mozilla Public License for details.
//
// This file contains code from https://github.com/google/certificate-transparency/tree/master/go
// See ct/AUTHORS and ct/LICENSE for copyright and license information.

package certspotter

import (
	//	"container/list"
	"crypto"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"software.sslmate.com/src/certspotter/ct"
	"software.sslmate.com/src/certspotter/ct/client"
)

type ProcessCallback func(*Scanner, *ct.LogEntry)

const (
	FETCH_RETRIES    = 10
	FETCH_RETRY_WAIT = 1
)

// ScannerOptions holds configuration options for the Scanner
type ScannerOptions struct {
	// Number of entries to request in one batch from the Log
	BatchSize int

	// Number of concurrent proecssors to run
	NumWorkers int

	// Don't print any status messages to stdout
	Quiet bool
}

// Creates a new ScannerOptions struct with sensible defaults
func DefaultScannerOptions() *ScannerOptions {
	return &ScannerOptions{
		BatchSize:  1000,
		NumWorkers: 1,
		Quiet:      false,
	}
}

// Scanner is a tool to scan all the entries in a CT Log.
type Scanner struct {
	// Base URI of CT log
	LogUri string

	// Public key of the log
	publicKey crypto.PublicKey

	// Client used to talk to the CT log instance
	logClient *client.LogClient

	// Configuration options for this Scanner instance
	opts ScannerOptions

	// Stats
	certsProcessed int64
}

// fetchRange represents a range of certs to fetch from a CT log
type fetchRange struct {
	start int64
	end   int64
}

// Worker function to process certs.
// Accepts ct.LogEntries over the |entries| channel, and invokes processCert on them.
// Returns true over the |done| channel when the |entries| channel is closed.
func (s *Scanner) processerJob(id int, entries <-chan ct.LogEntry, processCert ProcessCallback, wg *sync.WaitGroup) {
	for entry := range entries {
		atomic.AddInt64(&s.certsProcessed, 1)
		processCert(s, &entry)
	}
	wg.Done()
}

func (s *Scanner) fetch(r fetchRange, entries chan<- ct.LogEntry, treeBuilder *MerkleTreeBuilder) error {
	success := false
	retries := FETCH_RETRIES
	retryWait := FETCH_RETRY_WAIT
	for !success {
		s.Log(fmt.Sprintf("Fetching entries %d to %d", r.start, r.end))
		logEntries, err := s.logClient.GetEntries(r.start, r.end)
		if err != nil {
			if retries == 0 {
				s.Warn(fmt.Sprintf("Problem fetching entries %d to %d from log: %s", r.start, r.end, err.Error()))
				return err
			} else {
				s.Log(fmt.Sprintf("Problem fetching entries %d to %d from log (will retry): %s", r.start, r.end, err.Error()))
				time.Sleep(time.Duration(retryWait) * time.Second)
				retries--
				retryWait *= 2
				continue
			}
		}
		retries = FETCH_RETRIES
		retryWait = FETCH_RETRY_WAIT
		for _, logEntry := range logEntries {
			if treeBuilder != nil {
				treeBuilder.Add(hashLeaf(logEntry.LeafBytes))
			}
			logEntry.Index = r.start
			entries <- logEntry
			r.start++
		}
		if r.start > r.end {
			// Only complete if we actually got all the leaves we were
			// expecting -- Logs MAY return fewer than the number of
			// leaves requested.
			success = true
		}
	}
	return nil
}

// Worker function for fetcher jobs.
// Accepts cert ranges to fetch over the |ranges| channel, and if the fetch is
// successful sends the individual LeafInputs out into the
// |entries| channel for the processors to chew on.
// Will retry failed attempts to retrieve ranges indefinitely.
// Sends true over the |done| channel when the |ranges| channel is closed.
/* disabled becuase error handling is broken
func (s *Scanner) fetcherJob(id int, ranges <-chan fetchRange, entries chan<- ct.LogEntry, wg *sync.WaitGroup) {
	for r := range ranges {
		s.fetch(r, entries, nil)
	}
	wg.Done()
}
*/

// Returns the smaller of |a| and |b|
func min(a int64, b int64) int64 {
	if a < b {
		return a
	} else {
		return b
	}
}

// Returns the larger of |a| and |b|
func max(a int64, b int64) int64 {
	if a > b {
		return a
	} else {
		return b
	}
}

// Pretty prints the passed in number of |seconds| into a more human readable
// string.
func humanTime(seconds int) string {
	nanos := time.Duration(seconds) * time.Second
	hours := int(nanos / (time.Hour))
	nanos %= time.Hour
	minutes := int(nanos / time.Minute)
	nanos %= time.Minute
	seconds = int(nanos / time.Second)
	s := ""
	if hours > 0 {
		s += fmt.Sprintf("%d hours ", hours)
	}
	if minutes > 0 {
		s += fmt.Sprintf("%d minutes ", minutes)
	}
	if seconds > 0 {
		s += fmt.Sprintf("%d seconds ", seconds)
	}
	return s
}

func (s Scanner) Log(msg string) {
	if !s.opts.Quiet {
		log.Print(msg)
	}
}

func (s Scanner) Warn(msg string) {
	log.Print(msg)
}

func (s *Scanner) GetSTH() (*ct.SignedTreeHead, error) {
	latestSth, err := s.logClient.GetSTH()
	if err != nil {
		return nil, err
	}
	if s.publicKey != nil {
		verifier, err := ct.NewSignatureVerifier(s.publicKey)
		if err != nil {
			return nil, err
		}
		if err := verifier.VerifySTHSignature(*latestSth); err != nil {
			return nil, errors.New("STH signature is invalid: " + err.Error())
		}
	}
	return latestSth, nil
}

func (s *Scanner) CheckConsistency(first *ct.SignedTreeHead, second *ct.SignedTreeHead) (bool, *MerkleTreeBuilder, ct.ConsistencyProof, error) {
	var proof ct.ConsistencyProof

	if first.TreeSize > second.TreeSize {
		// No way this can be valid
		return false, nil, nil, nil
	} else if first.TreeSize == second.TreeSize {
		// The proof *should* be empty, so don't bother contacting the server.
		// This is necessary because the digicert server returns a 400 error if first==second.
		proof = []ct.MerkleTreeNode{}
	} else {
		var err error
		proof, err = s.logClient.GetConsistencyProof(int64(first.TreeSize), int64(second.TreeSize))
		if err != nil {
			return false, nil, nil, err
		}
	}

	valid, treeBuilder := VerifyConsistencyProof(proof, first, second)
	return valid, treeBuilder, proof, nil
}

func (s *Scanner) Scan(startIndex int64, endIndex int64, processCert ProcessCallback, treeBuilder *MerkleTreeBuilder) error {
	s.Log("Starting scan...")

	s.certsProcessed = 0
	startTime := time.Now()
	/* TODO: only launch ticker goroutine if in verbose mode; kill the goroutine when the scanner finishes
	ticker := time.NewTicker(time.Second)
	go func() {
		for range ticker.C {
			throughput := float64(s.certsProcessed) / time.Since(startTime).Seconds()
			remainingCerts := int64(endIndex) - int64(startIndex) - s.certsProcessed
			remainingSeconds := int(float64(remainingCerts) / throughput)
			remainingString := humanTime(remainingSeconds)
			s.Log(fmt.Sprintf("Processed: %d certs (to index %d). Throughput: %3.2f ETA: %s", s.certsProcessed,
				startIndex+int64(s.certsProcessed), throughput, remainingString))
		}
	}()
	*/

	// Start processor workers
	jobs := make(chan ct.LogEntry, 100)
	var processorWG sync.WaitGroup
	for w := 0; w < s.opts.NumWorkers; w++ {
		processorWG.Add(1)
		go s.processerJob(w, jobs, processCert, &processorWG)
	}

	for start := startIndex; start < int64(endIndex); {
		end := min(start+int64(s.opts.BatchSize), int64(endIndex)) - 1
		if err := s.fetch(fetchRange{start, end}, jobs, treeBuilder); err != nil {
			return err
		}
		start = end + 1
	}
	close(jobs)
	processorWG.Wait()
	s.Log(fmt.Sprintf("Completed %d certs in %s", s.certsProcessed, humanTime(int(time.Since(startTime).Seconds()))))

	return nil
}

// Creates a new Scanner instance using |client| to talk to the log, and taking
// configuration options from |opts|.
func NewScanner(logUri string, publicKey crypto.PublicKey, opts *ScannerOptions) *Scanner {
	var scanner Scanner
	scanner.LogUri = logUri
	scanner.publicKey = publicKey
	scanner.logClient = client.New(logUri)
	scanner.opts = *opts
	return &scanner
}
