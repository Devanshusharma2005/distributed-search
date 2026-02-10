package main

import (
	"context"
	"fmt"
	"log"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

func main() {
	log.Println("Connecting to etcd cluster...")


	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{"localhost:2379", "localhost:2381", "localhost:2383"},
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		log.Fatalf("Failed to connect to etcd: %v", err)
	}
	defer cli.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	log.Println("Writing test shard mapping...")
	_, err = cli.Put(ctx, "/shards/active/0", "localhost:8081")
	if err != nil {
		log.Fatalf("PUT failed: %v", err)
	}

	log.Println("Reading shard mapping...")
	resp, err := cli.Get(ctx, "/shards/active/0")
	if err != nil {
		log.Fatalf("GET failed: %v", err)
	}

	if len(resp.Kvs) == 0 {
		log.Fatal("Key not found!")
	}

	for _, kv := range resp.Kvs {
		fmt.Printf("etcd cluster working! Key: %s → Value: %s\n", kv.Key, kv.Value)
	}

	log.Println("Listing all /shards/active/* keys...")
	allShards, err := cli.Get(ctx, "/shards/active/", clientv3.WithPrefix())
	if err != nil {
		log.Fatalf("Prefix GET failed: %v", err)
	}

	fmt.Printf("Found %d shard(s):\n", len(allShards.Kvs))
	for _, kv := range allShards.Kvs {
		fmt.Printf("  - %s → %s\n", kv.Key, kv.Value)
	}

	log.Println("etcd test complete! Ready for Phase 2 shard registration.")
}