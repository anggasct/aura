package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"
)

func check(err error) {
	if err != nil {
		fmt.Println("failed:", err)
		os.Exit(1)
	}
}

func main() {
	dir, err := os.MkdirTemp("", "workload")
	check(err)
	check(os.MkdirAll(filepath.Join(dir, "x", "y"), 0o755))
	check(os.WriteFile(filepath.Join(dir, "a.txt"), bytes.Repeat([]byte("x"), 1000), 0o644))
	check(os.Rename(filepath.Join(dir, "a.txt"), filepath.Join(dir, "x", "b.txt")))
	in, err := os.Open(filepath.Join(dir, "x", "b.txt"))
	check(err)
	out, err := os.Create(filepath.Join(dir, "copy.txt"))
	check(err)
	_, err = io.Copy(out, in)
	check(err)
	check(out.Sync())
	check(syscall.Flock(int(out.Fd()), syscall.LOCK_EX))
	check(syscall.Fallocate(int(out.Fd()), 0, 0, 4096))
	check(out.Truncate(3))
	check(out.Close())
	check(in.Close())
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	check(tw.WriteHeader(&tar.Header{Name: "n", Mode: 0o600, Size: 2}))
	_, err = tw.Write([]byte("hi"))
	check(err)
	check(tw.Close())
	check(gz.Close())
	var zipped bytes.Buffer
	zw := zip.NewWriter(&zipped)
	member, err := zw.Create("m")
	check(err)
	_, err = member.Write([]byte("data"))
	check(err)
	check(zw.Close())
	var wg sync.WaitGroup
	results := make([]int, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sum := 0
			for n := range 2_000_000 {
				sum += n % (i + 2)
			}
			results[i] = sum
		}()
	}
	wg.Wait()
	sort.Ints(results)
	encoded, err := json.Marshal(map[string]any{"results": results, "digest": fmt.Sprintf("%x", sha256.Sum256(archive.Bytes()))[:8]})
	check(err)
	tpl := template.Must(template.New("t").Parse("{{.}}\n"))
	check(tpl.Execute(io.Discard, string(encoded)))
	factorial := new(big.Int).MulRange(1, 50)
	matched := regexp.MustCompile(`^\d+$`).MatchString(factorial.String())
	time.Sleep(10 * time.Millisecond)
	entries, err := os.ReadDir(dir)
	check(err)
	check(os.RemoveAll(dir))
	fmt.Println("ok", len(entries), matched, zipped.Len() > 0, strings.Count(string(encoded), "results"))
}
