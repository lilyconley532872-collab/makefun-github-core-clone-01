// push_bench_test.go — in-process timing of the server-side push path
// (readPack + applyPush), to see whether the push workload's ~1.5ms/push is
// server cost or HTTP/client overhead.
package main

import (
	"fmt"
	"testing"
	"time"
)

func TestPushPathTiming(t *testing.T) {
	s := NewStore()
	owner := "alice"
	repo := s.createRepo(&owner, "tok", "bench", false, "")
	// Seed: one commit on main.
	readmeOID := s.addObject(repo, &GitObject{Type: ObjBlob, Data: []byte("hello\n")})
	treeOID := s.addObject(repo, &GitObject{Type: ObjTree, Data: buildTree([]TreeEntry{{Mode: "100644", Name: "README.md", OID: readmeOID}})})
	commitOID := s.addObject(repo, &GitObject{Type: ObjCommit, Data: buildCommit(treeOID, nil, "init\n")})
	repo.Refs["refs/heads/main"] = commitOID

	start := time.Now()
	for i := 0; i < 200; i++ {
		// A 1 KiB blob + a tree (README + file.txt) + a commit on top of the tip,
		// exactly like the bench's buildPushPayloads.
		content := make([]byte, 1024)
		for j := range content {
			content[j] = byte('a' + (i+j)%26)
		}
		blobOID := hashObject(ObjBlob, content)
		treeData := buildTree([]TreeEntry{
			{Mode: "100644", Name: "README.md", OID: readmeOID},
			{Mode: "100644", Name: "file.txt", OID: blobOID},
		})
		treeOID := hashObject(ObjTree, treeData)
		parent := repo.Refs["refs/heads/main"]
		commitData := buildCommit(treeOID, []string{parent}, fmt.Sprintf("commit %d\n", i))
		commitOID := hashObject(ObjCommit, commitData)
		pack := writePack([]GitObject{
			{Type: ObjBlob, Data: content},
			{Type: ObjTree, Data: treeData},
			{Type: ObjCommit, Data: commitData},
		})
		cmd := fmt.Sprintf("%s %s refs/heads/main\x00report-status side-band-64k agent=test\n", parent, commitOID)
		body := append(pktLine([]byte(cmd)), pktEncode("flush")...)
		body = append(body, pack...)
		outcome := applyPush(s, repo, body)
		if !outcome.unpackOK || len(outcome.results) == 0 || !outcome.results[0].ok {
			t.Fatalf("push %d failed: %+v", i, outcome)
		}
	}
	elapsed := time.Since(start)
	t.Logf("200 sequential pushes (server-side only): %v total, %.1f µs/push", elapsed, float64(elapsed.Microseconds())/200)
}

func BenchmarkReadPack(b *testing.B) {
	content := make([]byte, 1024)
	for j := range content {
		content[j] = byte('a' + j%26)
	}
	blobOID := hashObject(ObjBlob, content)
	treeData := buildTree([]TreeEntry{
		{Mode: "100644", Name: "README.md", OID: blobOID},
		{Mode: "100644", Name: "file.txt", OID: blobOID},
	})
	treeOID := hashObject(ObjTree, treeData)
	commitData := buildCommit(treeOID, nil, "bench\n")
	pack := writePack([]GitObject{
		{Type: ObjBlob, Data: content},
		{Type: ObjTree, Data: treeData},
		{Type: ObjCommit, Data: commitData},
	})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := readPack(pack, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWritePack(b *testing.B) {
	content := make([]byte, 1024)
	for j := range content {
		content[j] = byte('a' + j%26)
	}
	blobOID := hashObject(ObjBlob, content)
	treeData := buildTree([]TreeEntry{
		{Mode: "100644", Name: "README.md", OID: blobOID},
		{Mode: "100644", Name: "file.txt", OID: blobOID},
	})
	treeOID := hashObject(ObjTree, treeData)
	commitData := buildCommit(treeOID, nil, "bench\n")
	objs := []GitObject{
		{Type: ObjBlob, Data: content},
		{Type: ObjTree, Data: treeData},
		{Type: ObjCommit, Data: commitData},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		writePack(objs)
	}
}
