/*
Copyright 2026 The Swarmada Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// robotLockedTimeout bounds the API calls made while a robot's lock is held: one attempt's read
// past the cache, its stop check and its status write. It is the API server's default request
// timeout, so no call the server would have answered is cut short here: a write that takes 20 s
// under a stop's load still lands, and the late-arrival pass still finds the robot. What it ends
// is a call on a connection that will never answer, which would otherwise hold the robot's lock,
// and the adapter's stream whose frame holds it, for good.
const robotLockedTimeout = 60 * time.Second

// robotLocks serializes, per robot, the decision and write of its zone (ZoneController.robotMu).
// Each robot has its own lock, so a slow write holds up only the robot's own frames, never
// another robot's or another adapter's. A lock exists only while a writer holds it or waits for
// it, so a robot that is forgotten leaves nothing behind. The zero value is ready to use.
type robotLocks struct {
	mu    sync.Mutex // guards locks; held only to find, count and remove an entry
	locks map[types.NamespacedName]*robotLock
}

// robotLock is one robot's lock and the number of writers holding it or waiting for it.
type robotLock struct {
	sync.Mutex
	users int
}

// lock takes robot's lock and returns its release.
func (l *robotLocks) lock(robot types.NamespacedName) func() {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = map[types.NamespacedName]*robotLock{}
	}
	rl := l.locks[robot]
	if rl == nil {
		rl = &robotLock{}
		l.locks[robot] = rl
	}
	rl.users++
	l.mu.Unlock()

	rl.Lock()
	return func() {
		rl.Unlock()
		l.mu.Lock()
		rl.users--
		if rl.users == 0 {
			delete(l.locks, robot)
		}
		l.mu.Unlock()
	}
}
