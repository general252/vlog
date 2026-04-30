package vlog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVLogBasic(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "vlog_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	vlog, err := Open(Options{
		DataDir:    tmpDir,
		MaxLogSize: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer vlog.Close()

	if err := vlog.Append([]byte("key1"), []byte("value1")); err != nil {
		t.Fatal(err)
	}
	if err := vlog.Append([]byte("key2"), []byte("value2")); err != nil {
		t.Fatal(err)
	}
	if err := vlog.Flush(); err != nil {
		t.Fatal(err)
	}

	reader, err := NewReader(vlog)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	entry, err := reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	if string(entry.Key) != "key1" || string(entry.Data) != "value1" {
		t.Errorf("Expected key1/value1, got %s/%s", entry.Key, entry.Data)
	}

	entry, err = reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	if string(entry.Key) != "key2" || string(entry.Data) != "value2" {
		t.Errorf("Expected key2/value2, got %s/%s", entry.Key, entry.Data)
	}
}

func TestCheckpoint(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "vlog_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	vlog, err := Open(Options{
		DataDir:    tmpDir,
		MaxLogSize: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 10; i++ {
		if err := vlog.Append([]byte("key"+string(rune('0'+i))), []byte("value"+string(rune('0'+i)))); err != nil {
			t.Fatal(err)
		}
	}
	vlog.Flush()
	vlog.Close()

	vlog, err = Open(Options{
		DataDir:    tmpDir,
		MaxLogSize: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer vlog.Close()

	reader, err := NewReader(vlog)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	for i := 0; i < 5; i++ {
		entry, err := reader.Read()
		if err != nil {
			t.Fatal(err)
		}
		if string(entry.Key) != "key"+string(rune('0'+i)) {
			t.Errorf("Expected key%d, got %s", i, entry.Key)
		}
	}

	if err := reader.UpdateCheckpoint(); err != nil {
		t.Fatal(err)
	}

	reader2, err := NewReader(vlog)
	if err != nil {
		t.Fatal(err)
	}
	defer reader2.Close()

	entry, err := reader2.Read()
	if err != nil {
		t.Fatal(err)
	}
	if string(entry.Key) != "key5" {
		t.Errorf("Expected key5, got %s", entry.Key)
	}
}

func TestFileRotation(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "vlog_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	vlog, err := Open(Options{
		DataDir:    tmpDir,
		MaxLogSize: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer vlog.Close()

	for i := 0; i < 20; i++ {
		if err := vlog.Append([]byte("key"+string(rune('0'+i))), []byte("value"+string(rune('0'+i)))); err != nil {
			t.Fatal(err)
		}
	}
	vlog.Flush()

	files, err := listVLogFiles(tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	if len(files) < 2 {
		t.Errorf("Expected at least 2 files, got %d", len(files))
	}
}

func TestCleanup(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "vlog_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	vlog, err := Open(Options{
		DataDir:    tmpDir,
		MaxLogSize: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer vlog.Close()

	for i := 0; i < 20; i++ {
		if err := vlog.Append([]byte("key"+string(rune('0'+i))), []byte("value"+string(rune('0'+i)))); err != nil {
			t.Fatal(err)
		}
	}
	vlog.Flush()

	reader, err := NewReader(vlog)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	for i := 0; i < 15; i++ {
		if _, err := reader.Read(); err != nil {
			t.Fatal(err)
		}
	}

	sr := reader.Stat()
	sw := vlog.Stat()
	count := sw.CurrentFileID - sr.CurrentFileID + 1

	if err = reader.UpdateCheckpoint(); err != nil {
		t.Fatal(err)
	}

	if err := reader.DeleteConsumedFiles(); err != nil {
		t.Fatal(err)
	}

	files, err := listVLogFiles(tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	if len(files) != int(count) {
		t.Errorf("Expected 1 remaining file, got %d", len(files))
	}
}

func TestRecovery(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "vlog_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	vlog, err := Open(Options{
		DataDir:    tmpDir,
		MaxLogSize: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 10; i++ {
		if err := vlog.Append([]byte("key"+string(rune('0'+i))), []byte("value"+string(rune('0'+i)))); err != nil {
			t.Fatal(err)
		}
	}
	vlog.Flush()

	reader, err := NewReader(vlog)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if _, err := reader.Read(); err != nil {
			t.Fatal(err)
		}
	}
	reader.UpdateCheckpoint()
	reader.Close()
	vlog.Close()

	vlog2, err := Open(Options{
		DataDir:    tmpDir,
		MaxLogSize: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer vlog2.Close()

	reader2, err := NewReader(vlog2)
	if err != nil {
		t.Fatal(err)
	}
	defer reader2.Close()

	entry, err := reader2.Read()
	if err != nil {
		t.Fatal(err)
	}
	if string(entry.Key) != "key3" {
		t.Errorf("Expected key3 after recovery, got %s", entry.Key)
	}
}

func TestConcurrency(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "vlog_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	vlog, err := Open(Options{
		DataDir:    tmpDir,
		MaxLogSize: 1024 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer vlog.Close()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			vlog.Append([]byte("key"+string(rune('0'+i))), []byte("value"+string(rune('0'+i))))
			time.Sleep(1 * time.Millisecond)
		}
		close(done)
	}()

	reader, err := NewReader(vlog)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	count := 0
	for {
		select {
		case <-done:
			for count < 100 {
				_, err := reader.Read()
				if err != nil {
					break
				}
				count++
			}
			return
		default:
			_, err := reader.Read()
			if err == ErrBlocked {
				time.Sleep(5 * time.Millisecond)
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			count++
		}
	}
}

func TestDirectoryLock(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "vlog_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	vlog1, err := Open(Options{
		DataDir:    tmpDir,
		MaxLogSize: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer vlog1.Close()

	_, err = Open(Options{
		DataDir:    tmpDir,
		MaxLogSize: 1024,
	})
	if err == nil {
		t.Error("Expected error when opening locked directory")
	}
}

func TestChecksumValidation(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "vlog_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	vlog, err := Open(Options{
		DataDir:    tmpDir,
		MaxLogSize: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer vlog.Close()

	vlog.Append([]byte("key1"), []byte("value1"))
	vlog.Flush()

	filename := filepath.Join(tmpDir, "00000001.vlog")
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}

	data[5] ^= 0xFF
	os.WriteFile(filename, data, 0644)

	reader, err := NewReader(vlog)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	_, err = reader.Read()
	if err != ErrChecksumMismatch {
		t.Errorf("Expected checksum error, got %v", err)
	}
}
