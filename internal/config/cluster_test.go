package config

import "testing"

func TestClusterExplicitPostgresAndPrivateTLS(t *testing.T) {
	good := Cluster{Enabled: true, NodeID: "node-a", Listen: "127.0.0.1:8443", AdvertiseURL: "https://node-a.example:8443", CAFile: "ca", CertFile: "cert", KeyFile: "key"}
	if err := good.validate("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := good.validate("sqlite"); err == nil {
		t.Fatal("cluster on SQLite")
	}
	for _, mutate := range []func(*Cluster){
		func(c *Cluster) { c.Enabled = false }, func(c *Cluster) { c.NodeID = "../node" }, func(c *Cluster) { c.KeyFile = "" }, func(c *Cluster) { c.Listen = "8443" }, func(c *Cluster) { c.AdvertiseURL = "http://node-a.example" }, func(c *Cluster) { c.AdvertiseURL = "https://node-a.example/path" }, func(c *Cluster) { c.AdvertiseURL = "https://node-a.example?" },
	} {
		c := good
		mutate(&c)
		if err := c.validate("postgres"); err == nil {
			t.Fatalf("invalid config accepted: %+v", c)
		}
	}
}
