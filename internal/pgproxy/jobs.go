// Copyright (c) 2026 MiniStack Contributors. SPDX-License-Identifier: MIT
// Copies or substantial portions, including AI-assisted ports or rewrites, must retain this notice (see LICENSE).
//
// Go port of the job registry (CREATE INDEX ASYNC / sys.jobs emulation) of
// ministack/core/pgproxy.py (MiniStack 1.5.9). MiniStack's LICENSE is
// reproduced in THIRD_PARTY_NOTICES at the root of this repository.

package pgproxy

import (
	"crypto/rand"
	"math/big"
	"strconv"
	"time"
)

// Job is one emulated asynchronous DDL job, as reported by sys.jobs.
type Job struct {
	JobID      string
	Status     string
	Details    string
	JobType    string
	ClassID    string
	ObjectID   string
	ObjectName string
	StartTime  string
	UpdateTime string
}

var jobColumns = []string{
	"job_id", "status", "details", "job_type", "class_id", "object_id",
	"object_name", "start_time", "update_time",
}

func isJobColumn(name string) bool {
	for _, c := range jobColumns {
		if c == name {
			return true
		}
	}
	return false
}

func (j Job) column(name string) string {
	switch name {
	case "job_id":
		return j.JobID
	case "status":
		return j.Status
	case "details":
		return j.Details
	case "job_type":
		return j.JobType
	case "class_id":
		return j.ClassID
	case "object_id":
		return j.ObjectID
	case "object_name":
		return j.ObjectName
	case "start_time":
		return j.StartTime
	case "update_time":
		return j.UpdateTime
	}
	return ""
}

const idAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

func randomInt(n int64) (int64, error) {
	v, err := rand.Int(rand.Reader, big.NewInt(n))
	if err != nil {
		return 0, err
	}
	return v.Int64(), nil
}

func newJob(objectName, jobType, status, details string) (Job, error) {
	id := make([]byte, 26)
	for i := range id {
		k, err := randomInt(int64(len(idAlphabet)))
		if err != nil {
			return Job{}, err
		}
		id[i] = idAlphabet[k]
	}
	oid, err := randomInt(100000)
	if err != nil {
		return Job{}, err
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00")
	return Job{
		JobID:      string(id),
		Status:     status,
		Details:    details,
		JobType:    jobType,
		ClassID:    "1259",
		ObjectID:   strconv.FormatInt(16384+oid, 10),
		ObjectName: objectName,
		StartTime:  now,
		UpdateTime: now,
	}, nil
}

// registerJob records a job in the proxy's registry.
func (p *Proxy) registerJob(objectName, jobType, status, details string) (Job, error) {
	job, err := newJob(objectName, jobType, status, details)
	if err != nil {
		return Job{}, err
	}
	p.mu.Lock()
	p.jobs = append(p.jobs, job)
	p.mu.Unlock()
	return job, nil
}

// Jobs returns a snapshot of the emulated sys.jobs registry.
func (p *Proxy) Jobs() []Job {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Job(nil), p.jobs...)
}

func (p *Proxy) bumpCatalog() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.catalogVersion++
	return p.catalogVersion
}

func (p *Proxy) currentCatalog() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.catalogVersion
}
