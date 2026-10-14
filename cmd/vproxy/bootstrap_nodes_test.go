package main

import (
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/node/exitpool"
)

func TestParseBootstrapNodes(t *testing.T) {
	const uri = "socks5://user:password@127.0.0.1:1080"
	nodes, err := parseBootstrapNodes(uri+"\n"+uri, true)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("dedup failed: count=%d err=%v", len(nodes), err)
	}
	if nodes[0].Disabled || strings.Contains(nodes[0].Name, "password") {
		t.Fatal("node should be enabled with a safe fallback name")
	}
	if nodes, err := parseBootstrapNodes("", false); err != nil || len(nodes) != 0 {
		t.Fatal("absent environment must be a no-op")
	}
	for _, input := range []string{"", " \n", "socks5://", "not-a-node-private-value",
		strings.Repeat("x", maxBootstrapNodesBytes+1),
		strings.Repeat(uri+"\n", maxBootstrapNodesCount+1)} {
		_, err := parseBootstrapNodes(input, true)
		if err == nil {
			t.Fatal("invalid bootstrap nodes accepted")
		}
		if strings.Contains(err.Error(), "not-a-node-private-value") {
			t.Fatal("error leaked node configuration")
		}
	}
}

func TestMergeBootstrapNodesPreservesExistingState(t *testing.T) {
	manager := exitpool.NewManager(nil, nil, exitpool.Hooks{}, nil)
	existing := exitpool.Node{Type: "socks", Name: "existing", RawURI: "socks5://127.0.0.1:1081"}
	manager.MergeNodes([]exitpool.Node{existing})
	nodes, err := parseBootstrapNodes("socks5://127.0.0.1:1080#bootstrap", true)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := mergeBootstrapNodes(manager, nodes); err != nil {
			t.Fatal(err)
		}
	}
	if len(manager.LoadNodes()) != 2 {
		t.Fatal("bootstrap should merge without removing or duplicating existing nodes")
	}
	manager.BatchUpdateNodesDisabled([]string{nodes[0].RawURI}, true)
	if err := mergeBootstrapNodes(manager, nodes); err == nil {
		t.Fatal("all bootstrap nodes disabled should cause a startup error")
	}
	for _, node := range manager.LoadNodes() {
		if node.RawURI == nodes[0].RawURI && !node.Disabled {
			t.Fatal("bootstrap re-enabled a manually disabled node")
		}
	}
}
