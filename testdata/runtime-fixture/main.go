// Runtime acceptance helper; it is excluded from production image/release inputs.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/calmcacil/sonarr-anime-bridge/internal/cache"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 2 {
		return fmt.Errorf("usage: fixture serve|seed|check|identity")
	}
	switch os.Args[1] {
	case "serve":
		encoder, err := zstd.NewWriter(nil)
		if err != nil {
			return err
		}
		defer encoder.Close()
		body := encoder.EncodeAll([]byte(`{"mal:16498":{"tvdb_show:12345:s1":{"1":"1"}}}`), nil)
		mux := http.NewServeMux()
		mux.HandleFunc("/mappings.json.zst", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("ETag", `"rootless-fixture-v1"`)
			if r.Method != http.MethodHead {
				_, _ = w.Write(body)
			}
		})
		mux.HandleFunc("/stall", func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
		mux.HandleFunc("/outage", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
		server := &http.Server{Addr: ":18080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		control := http.NewServeMux()
		control.HandleFunc("/disable", func(w http.ResponseWriter, _ *http.Request) {
			if err := server.Close(); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
		})
		controlServer := &http.Server{Addr: ":18081", Handler: control, ReadHeaderTimeout: 5 * time.Second}
		done := make(chan error, 2)
		go func() { done <- server.ListenAndServe() }()
		go func() { done <- controlServer.ListenAndServe() }()
		for range 2 {
			if err := <-done; err != http.ErrServerClosed {
				return err
			}
		}
		return nil
	case "disable-mapping":
		client := &http.Client{Timeout: 3 * time.Second}
		response, err := client.Get("http://127.0.0.1:18081/disable")
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("disable mapping: HTTP %d", response.StatusCode)
		}
		return nil
	case "crash":
		return syscall.Kill(1, syscall.SIGABRT)
	case "identity":
		status, err := os.ReadFile("/proc/self/status")
		if err != nil {
			return err
		}
		uidMap, err := os.ReadFile("/proc/self/uid_map")
		if err != nil {
			return err
		}
		gidMap, err := os.ReadFile("/proc/self/gid_map")
		if err != nil {
			return err
		}
		info, err := os.Stat("/data")
		if err != nil {
			return err
		}
		data := map[string]any{"uid": os.Getuid(), "gid": os.Getgid(), "uid_map": string(uidMap), "gid_map": string(gidMap), "data_mode": info.Mode().Perm()}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			data["data_uid"], data["data_gid"] = stat.Uid, stat.Gid
		}
		var filesystem unix.Statfs_t
		if err := unix.Statfs("/", &filesystem); err != nil {
			return err
		}
		if filesystem.Flags&unix.ST_RDONLY == 0 {
			return fmt.Errorf("container root mount is not read-only")
		}
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "CapEff:") || strings.HasPrefix(line, "NoNewPrivs:") {
				key, value, _ := strings.Cut(line, ":")
				data[key] = strings.TrimSpace(value)
			}
		}
		if err := os.WriteFile("/root-write-probe", []byte("unexpected writable root"), 0o600); err == nil {
			return fmt.Errorf("container root is writable")
		}
		probe, err := os.CreateTemp("/data", ".identity-probe-")
		if err != nil {
			return err
		}
		if err := probe.Close(); err != nil {
			return err
		}
		if err := os.Remove(probe.Name()); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(data)
	case "seed", "check":
		c, err := cache.Open("/data/cache.db")
		if err != nil {
			return err
		}
		operationErr := useCache(c, os.Args[1])
		closeErr := c.Close()
		if operationErr != nil {
			return operationErr
		}
		return closeErr
	default:
		return fmt.Errorf("unknown fixture command %q", os.Args[1])
	}
}

func useCache(c *cache.Cache, command string) error {
	year := time.Now().Year()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if command == "seed" {
		data := []byte(`[{"id":1,"idMal":16498,"title":{"english":"Rootless fixture"},"format":"TV","season":"SUMMER","duration":24}]`)
		if err := c.SetYearContext(ctx, year, data); err != nil {
			return err
		}
		return c.SetYearContext(ctx, year-1, []byte("[]"))
	}
	data, fresh, present, err := c.PeekYearContext(ctx, year)
	if err != nil {
		return err
	}
	if !fresh || !present || !strings.Contains(string(data), "Rootless fixture") {
		return fmt.Errorf("persisted year is missing or unusable")
	}
	count, err := c.CountSeenMappings(ctx)
	if err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("application did not persist mapping discovery in SQLite")
	}
	for _, name := range []string{"anibridge_mappings.json.zst", "anibridge_mappings.json.zst.meta.json"} {
		info, err := os.Stat(filepath.Join("/data", name))
		if err != nil {
			return err
		}
		if info.Size() == 0 {
			return fmt.Errorf("persisted %s is empty", name)
		}
	}
	return nil
}
