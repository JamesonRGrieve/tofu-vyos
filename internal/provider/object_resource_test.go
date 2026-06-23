// SPDX-License-Identifier: AGPL-3.0-or-later

package provider

import (
	"reflect"
	"sort"
	"testing"

	"github.com/JamesonRGrieve/tofu-vyos/internal/vyos"
)

func TestSubsetMatches(t *testing.T) {
	cases := []struct {
		name        string
		prior, cfg  string
		wantMatched bool
	}{
		{
			name:        "config subset of full device subtree — match (0-diff)",
			prior:       `{"address":"192.168.1.1/24","duplex":"auto","hw-id":"50:00:00:01:00:01","speed":"auto"}`,
			cfg:         `{"address":"192.168.1.1/24"}`,
			wantMatched: true,
		},
		{
			name:        "declared leaf drifted — no match (update)",
			prior:       `{"address":"192.168.1.254/24","duplex":"auto"}`,
			cfg:         `{"address":"192.168.1.1/24"}`,
			wantMatched: false,
		},
		{
			name:        "declared leaf missing on device — no match",
			prior:       `{"duplex":"auto"}`,
			cfg:         `{"address":"192.168.1.1/24"}`,
			wantMatched: false,
		},
		{
			name:        "key order / whitespace insensitive — match",
			prior:       `{"speed":"auto","address":"192.168.1.1/24"}`,
			cfg:         "{\n  \"address\": \"192.168.1.1/24\"\n}",
			wantMatched: true,
		},
		{
			name:        "nested sub-node subset — match",
			prior:       `{"global":{"facility":{"all":{"level":"info"},"protocols":{"level":"debug"}}}}`,
			cfg:         `{"global":{"facility":{"all":{"level":"info"}}}}`,
			wantMatched: true,
		},
		{
			name:        "nested leaf drift — no match",
			prior:       `{"global":{"facility":{"all":{"level":"info"}}}}`,
			cfg:         `{"global":{"facility":{"all":{"level":"warning"}}}}`,
			wantMatched: false,
		},
		{
			name:        "multi-value leaf (array) compared in order — match",
			prior:       `{"address":["10.10.10.10/24","10.10.10.11/24"],"description":"x"}`,
			cfg:         `{"address":["10.10.10.10/24","10.10.10.11/24"]}`,
			wantMatched: true,
		},
		{
			name:        "multi-value leaf order differs — match (sets are order-independent)",
			prior:       `{"address":["10.10.10.11/24","10.10.10.10/24"]}`,
			cfg:         `{"address":["10.10.10.10/24","10.10.10.11/24"]}`,
			wantMatched: true,
		},
		{
			name:        "multi-value leaf: declared subset of larger live list — match (re-IP leaving a stale address)",
			prior:       `{"address":["203.0.113.193/26","100.64.99.49/28"],"description":"LAN"}`,
			cfg:         `{"address":["100.64.99.49/28"]}`,
			wantMatched: true,
		},
		{
			name:        "multi-value leaf: declared element absent from live list — no match",
			prior:       `{"address":["10.10.10.10/24"]}`,
			cfg:         `{"address":["10.10.10.99/24"]}`,
			wantMatched: false,
		},
		{
			name:        "single-element array vs device scalar — match (VyOS single multi-value quirk)",
			prior:       `{"name-server":"100.64.92.1"}`,
			cfg:         `{"name-server":["100.64.92.1"]}`,
			wantMatched: true,
		},
		{
			name:        "two-element array vs device scalar — no match (second genuinely missing)",
			prior:       `{"name-server":"100.64.92.1"}`,
			cfg:         `{"name-server":["100.64.92.1","8.8.8.8"]}`,
			wantMatched: false,
		},
		{
			name:        "valueless node present — match",
			prior:       `{"dhcp":{},"description":"wan"}`,
			cfg:         `{"dhcp":{}}`,
			wantMatched: true,
		},
		{
			name:        "single-value leaf at root (host-name) — match",
			prior:       `"router1"`,
			cfg:         `"router1"`,
			wantMatched: true,
		},
		{
			name:        "single-value leaf at root drift — no match",
			prior:       `"router1"`,
			cfg:         `"router2"`,
			wantMatched: false,
		},
		{
			name:        "invalid prior JSON — no match (fall back to diff)",
			prior:       `not json`,
			cfg:         `{"a":1}`,
			wantMatched: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := subsetMatches(tc.prior, tc.cfg); got != tc.wantMatched {
				t.Fatalf("subsetMatches() = %v, want %v", got, tc.wantMatched)
			}
		})
	}
}

// sortedPaths flattens commands to their path arrays and sorts for comparison.
func sortedPaths(cmds []vyos.Command) [][]string {
	out := make([][]string, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, c.Path)
	}
	sort.Slice(out, func(i, j int) bool {
		return joinNul(out[i]) < joinNul(out[j])
	})
	return out
}

func joinNul(s []string) string {
	r := ""
	for _, x := range s {
		r += x + "\x00"
	}
	return r
}

func TestSetCommands(t *testing.T) {
	cases := []struct {
		name   string
		base   []string
		config string
		op     string
		want   [][]string
	}{
		{
			name:   "single-value leaf -> value as trailing segment",
			base:   []string{"system", "host-name"},
			config: `"router1"`,
			want:   [][]string{{"system", "host-name", "router1"}},
		},
		{
			name:   "object with single-value leaf",
			base:   []string{"interfaces", "ethernet", "eth1"},
			config: `{"address":"192.168.1.1/24","description":"lan"}`,
			want: [][]string{
				{"interfaces", "ethernet", "eth1", "address", "192.168.1.1/24"},
				{"interfaces", "ethernet", "eth1", "description", "lan"},
			},
		},
		{
			name:   "multi-value leaf -> one command per element",
			base:   []string{"interfaces", "dummy", "dum0"},
			config: `{"address":["10.10.10.10/24","10.10.10.11/24"]}`,
			want: [][]string{
				{"interfaces", "dummy", "dum0", "address", "10.10.10.10/24"},
				{"interfaces", "dummy", "dum0", "address", "10.10.10.11/24"},
			},
		},
		{
			name:   "nested sub-nodes recurse to leaves",
			base:   []string{"system", "syslog"},
			config: `{"global":{"facility":{"all":{"level":"info"}}}}`,
			want:   [][]string{{"system", "syslog", "global", "facility", "all", "level", "info"}},
		},
		{
			name:   "valueless node -> set the node path itself",
			base:   []string{"interfaces", "ethernet", "eth0", "address"},
			config: `"dhcp"`,
			want:   [][]string{{"interfaces", "ethernet", "eth0", "address", "dhcp"}},
		},
		{
			name:   "empty object node -> set node itself (tag present)",
			base:   []string{"service", "ssh"},
			config: `{}`,
			want:   [][]string{{"service", "ssh"}},
		},
		{
			name:   "numeric scalar rendered without trailing .0",
			base:   []string{"interfaces", "vxlan", "vxlan1", "vni"},
			config: `1`,
			want:   [][]string{{"interfaces", "vxlan", "vxlan1", "vni", "1"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmds, err := setCommands(tc.base, tc.config)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range cmds {
				if c.Op != "set" {
					t.Fatalf("op = %q, want set", c.Op)
				}
			}
			got := sortedPaths(cmds)
			want := tc.want
			sort.Slice(want, func(i, j int) bool { return joinNul(want[i]) < joinNul(want[j]) })
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("setCommands() = %v, want %v", got, want)
			}
		})
	}
}

func TestSetCommandsInvalidJSON(t *testing.T) {
	if _, err := setCommands([]string{"x"}, "not json"); err == nil {
		t.Fatal("expected error for invalid JSON config")
	}
}

func TestPruneCommands(t *testing.T) {
	base := []string{"interfaces", "ethernet", "eth1"}
	cases := []struct {
		name         string
		prior, newer string
		want         [][]string
	}{
		{
			name:  "leaf removed -> delete it",
			prior: `{"address":"192.168.1.1/24","description":"lan"}`,
			newer: `{"address":"192.168.1.1/24"}`,
			want:  [][]string{{"interfaces", "ethernet", "eth1", "description", "lan"}},
		},
		{
			name:  "nothing removed -> no deletes",
			prior: `{"address":"192.168.1.1/24"}`,
			newer: `{"address":"192.168.1.1/24","description":"lan"}`,
			want:  nil,
		},
		{
			name:  "one of a multi-value leaf removed -> delete that element",
			prior: `{"address":["10.0.0.1/24","10.0.0.2/24"]}`,
			newer: `{"address":["10.0.0.1/24"]}`,
			want:  [][]string{{"interfaces", "ethernet", "eth1", "address", "10.0.0.2/24"}},
		},
		{
			name:  "changed value -> old element deleted (new re-set elsewhere)",
			prior: `{"description":"old"}`,
			newer: `{"description":"new"}`,
			want:  [][]string{{"interfaces", "ethernet", "eth1", "description", "old"}},
		},
		{
			name:  "prior holds full device subtree, only managed leaves change",
			prior: `{"address":"192.168.1.1/24","duplex":"auto","speed":"auto"}`,
			newer: `{"address":"192.168.1.1/24","duplex":"auto","speed":"auto"}`,
			want:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmds, err := pruneCommands(base, tc.prior, tc.newer)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range cmds {
				if c.Op != "delete" {
					t.Fatalf("op = %q, want delete", c.Op)
				}
			}
			got := sortedPaths(cmds)
			want := tc.want
			sort.Slice(want, func(i, j int) bool { return joinNul(want[i]) < joinNul(want[j]) })
			if len(got) == 0 && len(want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("pruneCommands() = %v, want %v", got, want)
			}
		})
	}
}

func TestPruneCommandsUnparseablePriorIsBestEffort(t *testing.T) {
	cmds, err := pruneCommands([]string{"x"}, "not json", `{"a":"b"}`)
	if err != nil {
		t.Fatalf("expected nil error for unparseable prior, got %v", err)
	}
	if cmds != nil {
		t.Fatalf("expected no prune commands, got %v", cmds)
	}
}

func TestScalarString(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want string
	}{
		{"abc", "abc"},
		{float64(1), "1"},
		{float64(1500), "1500"},
		{float64(1.5), "1.5"},
		{true, "true"},
		{false, "false"},
	} {
		if got := scalarString(tc.in); got != tc.want {
			t.Errorf("scalarString(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCompactJSON(t *testing.T) {
	out, err := compactJSON([]byte("{\n \"b\": 2,\n \"a\": 1\n}"))
	if err != nil {
		t.Fatal(err)
	}
	// json.Marshal of a map sorts keys; whitespace is removed.
	if out != `{"a":1,"b":2}` {
		t.Fatalf("compactJSON = %q", out)
	}
}

func TestUnwrapLeafKeyed(t *testing.T) {
	cases := []struct {
		name    string
		segs    []string
		compact string
		want    string
	}{
		{
			name:    "value leaf keyed by node name — unwrap to scalar",
			segs:    []string{"system", "host-name"},
			compact: `{"host-name":"vyos-lab"}`,
			want:    `"vyos-lab"`,
		},
		{
			name:    "timezone value leaf — unwrap",
			segs:    []string{"system", "time-zone"},
			compact: `{"time-zone":"America/Edmonton"}`,
			want:    `"America/Edmonton"`,
		},
		{
			name:    "multi-value leaf keyed by node name — unwrap to array",
			segs:    []string{"system", "name-server"},
			compact: `{"name-server":["1.1.1.1","8.8.8.8"]}`,
			want:    `["1.1.1.1","8.8.8.8"]`,
		},
		{
			name:    "container subtree (multiple keys) — left as-is",
			segs:    []string{"service", "https"},
			compact: `{"api":{"rest":{}},"port":"443"}`,
			want:    `{"api":{"rest":{}},"port":"443"}`,
		},
		{
			name:    "single-key object whose key is NOT the leaf — left as-is",
			segs:    []string{"service"},
			compact: `{"https":{"port":"443"}}`,
			want:    `{"https":{"port":"443"}}`,
		},
		{
			name:    "already-scalar response — left as-is",
			segs:    []string{"system", "host-name"},
			compact: `"vyos-lab"`,
			want:    `"vyos-lab"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := unwrapLeafKeyed(tc.segs, tc.compact); got != tc.want {
				t.Fatalf("unwrapLeafKeyed() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNotFoundClassification(t *testing.T) {
	for _, tc := range []struct {
		msg  string
		want bool
	}{
		{"Configuration under specified path is empty", true},
		{"specified path is not valid", true},
		{"path does not exist", true},
		{"Invalid value", false},
		{"commit failed", false},
	} {
		err := &vyos.APIError{Endpoint: "/retrieve", Status: 400, Message: tc.msg}
		if got := vyos.NotFound(err); got != tc.want {
			t.Errorf("NotFound(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
	if vyos.NotFound(nil) {
		t.Error("NotFound(nil) should be false")
	}
}
