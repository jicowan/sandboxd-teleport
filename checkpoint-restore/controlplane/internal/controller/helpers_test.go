/*
Copyright 2026.

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

import "testing"

func TestBusyCount(t *testing.T) {
	cases := []struct {
		name        string
		idle, total int
		want        int32
	}{
		{"typical", 2, 5, 3},
		{"all idle", 4, 4, 0},
		{"all busy", 0, 3, 3},
		{"empty pool", 0, 0, 0},
		// KV drift: a stale idle-set member makes idle > total. Must floor at 0,
		// never return negative (which would under-provision effReplicas).
		{"drift idle>total", 5, 4, 0},
	}
	for _, c := range cases {
		if got := busyCount(c.idle, c.total); got != c.want {
			t.Errorf("%s: busyCount(idle=%d,total=%d)=%d, want %d", c.name, c.idle, c.total, got, c.want)
		}
	}
}
