package config

import "testing"

func TestExplicitNASSetupCanStartWithNoMediaRoots(t *testing.T) {
	cfg, e := Parse("[hub]\nsetup=true\n")
	if e != nil || !cfg.Hub.Setup || len(cfg.Hub.Storage) != 0 {
		t.Fatal("empty explicit NAS setup rejected", e)
	}
	if _, e = Parse("[hub]\nsetup=false\n"); e == nil {
		t.Fatal("legacy empty config unexpectedly accepted")
	}
}
