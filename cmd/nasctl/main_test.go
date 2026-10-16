package main

import (
	"reflect"
	"testing"
)

func TestAlbumCommandAcceptsLeadingGlobalFlags(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want []string
		ok   bool
	}{
		{[]string{"album", "status"}, []string{"status"}, true},
		{[]string{"--config", "/etc/x.toml", "album", "scan", "--limit", "5"}, []string{"scan", "--limit", "5", "--config", "/etc/x.toml"}, true},
		{[]string{"-c", "x.toml", "--json", "album", "status"}, []string{"status", "--config", "x.toml", "--json"}, true},
		{[]string{"--config=x.toml", "album", "doctor"}, []string{"doctor", "--config=x.toml"}, true},
		{[]string{"status"}, nil, false},
		{[]string{"--config", "x.toml", "status", "album"}, nil, false},
		{[]string{"exec-album-task", "album", "thumbs"}, nil, false},
		{[]string{"--config"}, nil, false},
	} {
		got, ok := albumCommand(tc.in)
		if ok != tc.ok || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("albumCommand(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
