package vlog

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

// 常量定义
const (
	magicNumber   = 0x564C4F47        // 魔数，用于验证日志文件的有效性 (VLOG的ASCII码)
	defaultMaxLog = 512 * 1024 * 1024 // 默认单个日志文件最大大小 512MB
	fileExt       = ".vlog"           // 日志文件扩展名
	metaFile      = "meta.mt"         // 元数据文件
	metaTmpFile   = "meta.mt.tmp"     // 元数据临时文件
	lockFile      = "vlog.lock"       // 目录锁文件
)

// 错误变量定义
var (
	ErrChecksumMismatch  = errors.New("checksum mismatch")                     // 校验和不匹配错误
	ErrMagicNumberWrong  = errors.New("invalid magic number")                  // 魔数错误
	ErrReadOffsetInvalid = errors.New("read offset exceeds file size")         // 读取偏移量超出文件大小
	ErrFileNotFound      = errors.New("file not found")                        // 文件未找到错误
	ErrBlocked           = errors.New("reader blocked, caught up with writer") // 读取器阻塞错误
)

type Options struct {
	DataDir    string
	MaxLogSize int64
}

func DefaultOptions(path string) Options {
	return Options{
		DataDir:    path,
		MaxLogSize: defaultMaxLog,
	}
}

// Entry 表示日志条目，包含键和数据
type Entry struct {
	Key  []byte // 条目的键
	Data []byte // 条目的数据
}

// VLog 是日志存储引擎的主要结构
type VLog struct {
	mu               sync.Mutex   // 互斥锁，保护并发访问
	dataDir          string       // 数据目录路径
	maxLogSize       int64        // 单个日志文件的最大大小
	activeFile       *os.File     // 当前活跃的日志文件
	activeFileID     uint64       // 当前活跃文件的ID
	activeFileOffset int64        // 当前活跃文件的写入偏移量
	dirLock          *flock.Flock // 目录锁，防止多个实例同时运行
	closed           bool         // 标识VLog是否已关闭
	writeCond        *sync.Cond   // 条件变量，用于通知读取器有新数据写入

	reader *Reader
}

// Open 打开或创建一个VLog实例
// dataDir: 数据目录路径
// maxLogSize: 单个日志文件的最大大小（可选，默认为512MB）
// 返回VLog实例和错误信息
func Open(opts Options) (*VLog, error) {
	size := opts.MaxLogSize
	if opts.MaxLogSize > 0 {
		size = opts.MaxLogSize
	}

	// 创建数据目录（如果不存在）
	if err := os.MkdirAll(opts.DataDir, 0755); err != nil {
		log.Println(err)
		return nil, err
	}

	// 创建目录锁，防止多个实例同时运行
	lock := flock.New(filepath.Join(opts.DataDir, lockFile))
	locked, err := lock.TryLock()
	if err != nil {
		log.Println(err)
		return nil, err
	}
	if !locked {
		log.Println("another instance is running")
		return nil, errors.New("another instance is running")
	}

	// 初始化VLog实例
	v := &VLog{
		dataDir:    opts.DataDir,
		maxLogSize: size,
		dirLock:    lock,
		writeCond:  sync.NewCond(&sync.Mutex{}),
	}

	// 加载当前活跃的日志文件
	if err = v.loadActiveFile(); err != nil {
		_ = lock.Unlock()
		log.Println(err)
		return nil, err
	}

	return v, nil
}

// loadActiveFile 加载当前活跃的日志文件
// 如果目录为空则创建新文件，否则加载最后一个日志文件
func (v *VLog) loadActiveFile() error {
	files, err := listVLogFiles(v.dataDir)
	if err != nil {
		log.Println(err)
		return err
	}

	// 如果没有日志文件，创建第一个文件
	if len(files) == 0 {
		return v.createNewActiveFile(1)
	}

	// 获取最后一个日志文件
	lastFile := files[len(files)-1]
	fileID := parseFileID(lastFile)

	// 以读写模式打开文件
	f, err := os.OpenFile(filepath.Join(v.dataDir, lastFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Println(err)
		return err
	}

	// 获取文件状态以确定写入偏移量
	stat, err := f.Stat()
	if err != nil {
		_ = f.Close()
		log.Println(err)
		return err
	}

	// 设置当前活跃文件的相关属性
	v.activeFile = f
	v.activeFileID = fileID
	v.activeFileOffset = stat.Size()

	return nil
}

// createNewActiveFile 创建一个新的活跃日志文件
// fileID: 新文件的ID
func (v *VLog) createNewActiveFile(fileID uint64) error {
	// 生成文件名，格式为 00000001.vlog
	filename := fmt.Sprintf("%08d%s", fileID, fileExt)
	path := filepath.Join(v.dataDir, filename)

	// 创建并打开文件（如果存在则截断）
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}

	// 更新当前活跃文件的相关属性
	v.activeFile = f
	v.activeFileID = fileID
	v.activeFileOffset = 0

	return nil
}

// rotateIfNeeded 检查是否需要进行文件轮转
// 如果当前活跃文件大小超过限制，则关闭当前文件并创建新文件
func (v *VLog) rotateIfNeeded() error {
	// 如果当前文件大小未超过限制，不需要轮转
	if v.activeFileOffset < v.maxLogSize {
		return nil
	}

	// 同步文件到磁盘
	if err := v.activeFile.Sync(); err != nil {
		return err
	}
	// 关闭当前文件
	if err := v.activeFile.Close(); err != nil {
		return err
	}

	// 创建新的活跃文件
	return v.createNewActiveFile(v.activeFileID + 1)
}

// Append 向日志文件追加一条记录
// key: 条目的键
// data: 条目的数据
// 返回错误信息
func (v *VLog) Append(key, data []byte) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	// 检查VLog是否已关闭
	if v.closed {
		return errors.New("vlog is closed")
	}

	// 检查是否需要轮转文件
	if err := v.rotateIfNeeded(); err != nil {
		return err
	}

	// 计算条目大小：魔数(4) + CRC(4) + 键长度(4) + 值长度(4) + 键数据 + 值数据
	entrySize := 4 + 4 + 4 + 4 + len(key) + len(data)

	// 检查当前文件是否有足够空间容纳新条目
	if v.activeFileOffset+int64(entrySize) > v.maxLogSize {
		// 同步文件到磁盘
		if err := v.activeFile.Sync(); err != nil {
			return err
		}
		// 关闭当前文件
		if err := v.activeFile.Close(); err != nil {
			return err
		}
		// 创建新文件
		if err := v.createNewActiveFile(v.activeFileID + 1); err != nil {
			return err
		}
	}

	// 创建条目并写入
	entry := &Entry{Key: key, Data: data}
	if err := writeEntry(v.activeFile, entry); err != nil {
		return err
	}

	// 更新写入偏移量
	v.activeFileOffset += int64(entrySize)
	// 通知所有等待的读取器有新数据写入
	v.writeCond.Broadcast()

	return nil
}

// writeEntry 将日志条目写入到writer中
// 条目格式：魔数(4) + CRC(4) + 键长度(4) + 值长度(4) + 键数据 + 值数据
func writeEntry(w io.Writer, entry *Entry) error {
	// 计算缓冲区大小：16字节头部 + 键长度 + 值长度
	buf := make([]byte, 16+len(entry.Key)+len(entry.Data))

	// 写入魔数
	binary.LittleEndian.PutUint32(buf[0:4], magicNumber)

	// 写入键长度（位置8:12）
	binary.LittleEndian.PutUint32(buf[8:12], uint32(len(entry.Key)))

	// 写入值长度（位置12:16）
	binary.LittleEndian.PutUint32(buf[12:16], uint32(len(entry.Data)))

	// 写入键数据（位置16开始）
	copy(buf[16:16+len(entry.Key)], entry.Key)

	// 写入值数据（位置16+键长度开始）
	copy(buf[16+len(entry.Key):], entry.Data)

	// 计算CRC校验值（仅对键和值数据部分，位置16开始）
	dataForCRC := buf[16:]
	crc := crc32.ChecksumIEEE(dataForCRC)

	// 写入CRC（位置4:8）
	binary.LittleEndian.PutUint32(buf[4:8], crc)

	// 写入缓冲区到writer
	_, err := w.Write(buf)
	return err
}

// Flush 将缓冲区中的数据刷新到磁盘
// 返回错误信息
func (v *VLog) Flush() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	// 检查VLog是否已关闭
	if v.closed {
		return errors.New("vlog is closed")
	}

	// 将文件内容同步到磁盘
	return v.activeFile.Sync()
}

// Close 关闭VLog实例，释放资源
// 返回错误信息
func (v *VLog) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	// 如果已经关闭，直接返回
	if v.closed {
		return nil
	}

	// 标记为已关闭
	v.closed = true

	// 将文件内容同步到磁盘
	if err := v.activeFile.Sync(); err != nil {
		return err
	}
	// 关闭活跃文件
	if err := v.activeFile.Close(); err != nil {
		return err
	}

	// 通知所有等待的读取器
	v.writeCond.Broadcast()

	// 释放目录锁
	return v.dirLock.Unlock()
}

type WriteState struct {
	CurrentFileID uint64 // 当前文件的ID
	WriteOffset   int64  // 当前写入位置
}

// Stat 获取当前写入位置
// 返回当前活跃文件ID和文件偏移量
func (v *VLog) Stat() WriteState {
	v.mu.Lock()
	defer v.mu.Unlock()

	return WriteState{
		CurrentFileID: v.activeFileID,
		WriteOffset:   v.activeFileOffset,
	}
}

// WaitForData 等待新数据写入
// timeout: 超时时间，如果为0则无限等待
// 返回是否在超时前收到新数据通知
func (v *VLog) WaitForData(timeout time.Duration) bool {
	v.writeCond.L.Lock()
	defer v.writeCond.L.Unlock()

	if timeout > 0 {
		// 使用带超时的等待
		done := make(chan struct{})
		go func() {
			v.writeCond.Wait()
			close(done)
		}()
		select {
		case <-done:
			return true
		case <-time.After(timeout):
			return false
		}
	} else {
		// 无限等待直到有新数据
		v.writeCond.Wait()
		return true
	}
}

func (v *VLog) NewReader() (*Reader, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.reader != nil {
		return v.reader, nil
	}

	if r, err := NewReader(v); err != nil {
		return nil, err
	} else {
		v.reader = r
	}

	return v.reader, nil
}
