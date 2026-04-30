package vlog

import (
	"bytes"           // 用于字节缓冲区操作
	"encoding/binary" // 用于二进制数据的编解码
	"errors"          // 用于错误处理
	"hash/crc32"      // 用于CRC32校验
	"io"              // 用于IO接口
	"os"              // 用于操作系统相关操作
	"path/filepath"   // 用于文件路径操作
)

// Checkpoint 表示读取检查点，记录当前读取位置
type Checkpoint struct {
	CurrentFileID uint64 // 当前读取的文件ID
	ReadOffset    int64  // 当前文件的读取偏移量
}

// Load 从数据目录加载检查点信息
// dataDir: 数据目录路径
// 返回错误信息
func (c *Checkpoint) Load(dataDir string) error {
	path := filepath.Join(dataDir, metaFile)
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	// 将文件内容读取到缓冲区
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, f); err != nil {
		return err
	}

	data := buf.Bytes()
	// 检查文件大小是否有效（至少需要20字节：CRC(4) + FileID(8) + Offset(8)）
	if len(data) < 20 {
		return errors.New("invalid checkpoint file")
	}

	// 解析检查点数据
	storedCRC := binary.LittleEndian.Uint32(data[0:4])            // 读取存储的CRC值
	c.CurrentFileID = binary.LittleEndian.Uint64(data[4:12])      // 读取当前文件ID
	c.ReadOffset = int64(binary.LittleEndian.Uint64(data[12:20])) // 读取读取偏移量

	// 验证CRC校验值
	dataForCRC := data[4:20]
	computedCRC := crc32.ChecksumIEEE(dataForCRC)
	if computedCRC != storedCRC {
		return ErrChecksumMismatch
	}

	return nil
}

// Save 将检查点信息保存到数据目录
// 使用原子写入方式：先写入临时文件，再重命名为正式文件
// dataDir: 数据目录路径
// 返回错误信息
func (c *Checkpoint) Save(dataDir string) error {
	// 创建20字节的缓冲区：CRC(4) + FileID(8) + Offset(8)
	buf := make([]byte, 20)
	// 写入文件ID和偏移量
	binary.LittleEndian.PutUint64(buf[4:12], c.CurrentFileID)
	binary.LittleEndian.PutUint64(buf[12:20], uint64(c.ReadOffset))

	// 计算并写入CRC校验值
	dataForCRC := buf[4:20]
	crc := crc32.ChecksumIEEE(dataForCRC)
	binary.LittleEndian.PutUint32(buf[0:4], crc)

	// 构建文件路径
	tmpPath := filepath.Join(dataDir, metaTmpFile)
	metaPath := filepath.Join(dataDir, metaFile)

	// 创建临时文件
	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}

	// 写入缓冲区内容
	if _, err := f.Write(buf); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return err
	}

	// 同步到磁盘，确保数据持久化
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return err
	}

	// 关闭文件
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	// 原子重命名，确保文件一致性
	return os.Rename(tmpPath, metaPath)
}
