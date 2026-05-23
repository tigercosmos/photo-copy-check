package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

type result struct {
	relPath string
	status  string // "missing", "size_mismatch", "hash_mismatch", "read_error"
	detail  string
}

var imageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true,
	".bmp": true, ".tif": true, ".tiff": true, ".webp": true,
	".heic": true, ".heif": true, ".raw": true, ".cr2": true,
	".nef": true, ".arw": true, ".dng": true, ".orf": true,
	".rw2": true, ".raf": true, ".srw": true, ".pef": true,
}

func isImage(name string) bool {
	return imageExts[strings.ToLower(filepath.Ext(name))]
}

func hashFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// resolveDir resolves symlinks on the given path and confirms it points at a
// directory. filepath.Walk uses Lstat on the root, so passing a symlinked
// directory would otherwise be skipped silently.
func resolveDir(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", resolved)
	}
	return resolved, nil
}

func collectFiles(root string, imagesOnly bool) ([]string, error) {
	var files []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if imagesOnly && !isImage(info.Name()) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, rel)
		return nil
	})
	return files, err
}

func checkOne(rel, src, dst string, quickSize bool) *result {
	srcPath := filepath.Join(src, rel)
	dstPath := filepath.Join(dst, rel)

	si, err := os.Stat(srcPath)
	if err != nil {
		return &result{rel, "read_error", fmt.Sprintf("source: %v", err)}
	}
	di, err := os.Stat(dstPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &result{rel, "missing", "not present in copy"}
		}
		return &result{rel, "read_error", fmt.Sprintf("copy: %v", err)}
	}
	if si.Size() != di.Size() {
		return &result{rel, "size_mismatch",
			fmt.Sprintf("src=%d bytes, copy=%d bytes", si.Size(), di.Size())}
	}
	if quickSize {
		return nil
	}
	sh, err := hashFile(srcPath)
	if err != nil {
		return &result{rel, "read_error", fmt.Sprintf("hash source: %v", err)}
	}
	dh, err := hashFile(dstPath)
	if err != nil {
		return &result{rel, "read_error", fmt.Sprintf("hash copy: %v", err)}
	}
	if string(sh) != string(dh) {
		return &result{rel, "hash_mismatch", "content differs"}
	}
	return nil
}

func main() {
	src := flag.String("src", "", "source directory")
	dst := flag.String("dst", "", "copy (destination) directory to verify against the source")
	workers := flag.Int("workers", runtime.NumCPU(), "number of parallel workers")
	imagesOnly := flag.Bool("images-only", true, "only check known image extensions")
	quickSize := flag.Bool("quick", false, "compare file size only (skip hashing — fast but weaker)")
	flag.Parse()

	if *src == "" || *dst == "" {
		fmt.Fprintln(os.Stderr, "usage: photo_check -src <source-dir> -dst <copy-dir> [-workers N] [-images-only=false] [-quick]")
		os.Exit(2)
	}
	if *workers < 1 {
		fmt.Fprintf(os.Stderr, "workers must be >= 1 (got %d)\n", *workers)
		os.Exit(2)
	}

	srcResolved, err := resolveDir(*src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "source is not a readable directory: %v\n", err)
		os.Exit(2)
	}
	dstResolved, err := resolveDir(*dst)
	if err != nil {
		fmt.Fprintf(os.Stderr, "destination is not a readable directory: %v\n", err)
		os.Exit(2)
	}

	fmt.Printf("Scanning source: %s\n", srcResolved)
	files, err := collectFiles(srcResolved, *imagesOnly)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to scan source: %v\n", err)
		os.Exit(1)
	}
	total := int64(len(files))
	fmt.Printf("Found %d file(s). Verifying with %d worker(s)...\n", total, *workers)

	jobs := make(chan string, *workers*2)
	var problems []result
	var mu sync.Mutex
	var done int64

	var wg sync.WaitGroup
	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for rel := range jobs {
				if r := checkOne(rel, srcResolved, dstResolved, *quickSize); r != nil {
					mu.Lock()
					problems = append(problems, *r)
					mu.Unlock()
				}
				d := atomic.AddInt64(&done, 1)
				if d == total || d%100 == 0 {
					fmt.Fprintf(os.Stderr, "\rchecked %d / %d", d, total)
				}
			}
		}()
	}

	for _, rel := range files {
		jobs <- rel
	}
	close(jobs)
	wg.Wait()
	fmt.Fprintln(os.Stderr)

	if len(problems) == 0 {
		fmt.Printf("OK: all %d file(s) match.\n", total)
		return
	}

	sort.Slice(problems, func(i, j int) bool {
		if problems[i].status != problems[j].status {
			return problems[i].status < problems[j].status
		}
		return problems[i].relPath < problems[j].relPath
	})

	counts := map[string]int{}
	for _, p := range problems {
		counts[p.status]++
	}

	fmt.Printf("FAIL: %d file(s) had issues out of %d.\n", len(problems), total)
	for _, k := range []string{"missing", "size_mismatch", "hash_mismatch", "read_error"} {
		if counts[k] > 0 {
			fmt.Printf("  %s: %d\n", k, counts[k])
		}
	}
	fmt.Println()
	for _, p := range problems {
		fmt.Printf("[%s] %s  (%s)\n", p.status, p.relPath, p.detail)
	}
	os.Exit(1)
}
