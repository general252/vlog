package vlog

import (
	"encoding/binary" // 用于二进制数据的编解码
	"fmt"             // 用于格式化输出
	"hash/crc32"      // 用于CRC32校验
	"io"              // 用于IO接口
	"log"
	"os"            // 用于操作系统相关操作
	"path/filepath" // 用于文件路径操作
	"sort"          // 用于排序
	"strconv"       // 用于字符串转换
	"strings"       // 用于字符串操作
)

// Reader 表示日志读取器，用于从vlog中顺序读取条目
type Reader struct {
	vlog          *VLog       // 关联的VLog实例
	currentFile   *os.File    // 当前正在读取的文件
	currentFileID uint64      // 当前文件的ID
	readOffset    int64       // 当前读取位置
	checkpoint    *Checkpoint // 检查点，用于持久化读取位置
}

// NewReader 创建一个新的日志读取器
// vlog: 关联的VLog实例
// 返回Reader实例和错误信息
func NewReader(vlog *VLog) (*Reader, error) {
	r := &Reader{
		vlog: vlog,
		checkpoint: &Checkpoint{
			CurrentFileID: 1, // 默认从第一个文件开始
			ReadOffset:    0, // 默认从文件开头开始
		},
	}

	// 尝试加载检查点，如果检查点文件不存在则忽略
	if err := r.loadCheckpoint(); err != nil {
		if !os.IsNotExist(err) {
			log.Println(err)
			return nil, err
		}
	}

	// 打开当前文件
	if err := r.openCurrentFile(); err != nil {
		log.Println(err)
		return nil, err
	}

	return r, nil
}

// loadCheckpoint 从数据目录加载检查点
func (r *Reader) loadCheckpoint() error {
	return r.checkpoint.Load(r.vlog.dataDir)
}

// openCurrentFile 打开当前检查点指向的文件
// 如果当前已有打开的文件，先关闭它
// 支持崩溃恢复：若文件不存在或偏移量无效，自动定位到有效位置
func (r *Reader) openCurrentFile() error {
	if r.currentFile != nil {
		_ = r.currentFile.Close()
	}

	// 获取所有日志文件列表
	files, err := listVLogFiles(r.vlog.dataDir)
	if err != nil {
		log.Println(err)
		return err
	}

	if len(files) == 0 {
		log.Println("No files found in data directory")
		return ErrFileNotFound
	}

	// 获取当前检查点指向的文件名
	targetFilename := fmt.Sprintf("%08d%s", r.checkpoint.CurrentFileID, fileExt)
	targetPath := filepath.Join(r.vlog.dataDir, targetFilename)

	// 检查目标文件是否存在
	f, err := os.Open(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			// 崩溃恢复：检查点指向的文件已被删除，定位到最小编号文件
			firstFile := files[0]
			firstFileID := parseFileID(firstFile)
			r.checkpoint.CurrentFileID = firstFileID
			r.checkpoint.ReadOffset = 0
			return r.openCurrentFile()
		}
		log.Println(err)
		return err
	}

	// 获取文件大小，检查偏移量是否有效
	stat, err := f.Stat()
	if err != nil {
		_ = f.Close()
		log.Println(err)
		return err
	}

	// 崩溃恢复：若偏移量超过文件大小，跳转到下一个文件
	if r.checkpoint.ReadOffset > stat.Size() {
		_ = f.Close()
		return r.advanceToNextFile()
	}

	// 定位到检查点记录的偏移位置
	if _, err = f.Seek(r.checkpoint.ReadOffset, io.SeekStart); err != nil {
		_ = f.Close()
		log.Println(err)
		return err
	}

	// 更新读取器状态
	r.currentFile = f
	r.currentFileID = r.checkpoint.CurrentFileID
	r.readOffset = r.checkpoint.ReadOffset

	return nil
}

// Read 读取下一条日志条目
// 如果当前文件读完，自动切换到下一个文件
// 返回读取的条目和错误信息
func (r *Reader) Read() (*Entry, error) {
	for {
		entry, offset, err := r.readEntry()
		// 如果当前文件读完，尝试切换到下一个文件
		if err == io.EOF {
			if err := r.advanceToNextFile(); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, err
		}

		// 更新读取偏移量
		r.readOffset = offset
		return entry, nil
	}
}

// readEntry 从当前文件读取单个日志条目
// 条目格式：魔数(4) + CRC(4) + 键长度(4) + 值长度(4) + 键数据 + 值数据
// 返回读取的条目、当前位置和错误信息
func (r *Reader) readEntry() (*Entry, int64, error) {
	if r.currentFile == nil {
		log.Println("Current file is nil")
		return nil, 0, ErrFileNotFound
	}

	// 检查指针约束：读取位置不能超过写入位置
	// 获取当前写入位置
	state := r.vlog.Stat()

	// 如果当前读取的文件不是活跃文件，说明后面的文件都还没有数据
	if r.currentFileID < state.CurrentFileID {
		// 当前文件不是活跃文件，可以继续读取
	} else if r.currentFileID == state.CurrentFileID {
		// 当前文件是活跃文件，检查读取位置是否超过写入位置
		// 单个条目的最小大小：16字节头部
		if r.readOffset+16 > state.WriteOffset {
			return nil, 0, ErrBlocked
		}
	} else {
		// 不应该发生：读取的文件ID大于写入的文件ID
		return nil, 0, ErrBlocked
	}

	// 读取16字节头部：魔数(4) + CRC(4) + 键长度(4) + 值长度(4)
	header := make([]byte, 16)
	n, err := io.ReadFull(r.currentFile, header)
	if err != nil {
		if err == io.EOF {
			return nil, 0, err
		}
		log.Println(err)
		return nil, 0, err
	}
	if n != 16 {
		return nil, 0, io.EOF
	}

	// 验证魔数
	magic := binary.LittleEndian.Uint32(header[0:4])
	if magic != magicNumber {
		log.Println("Magic number not matched")
		return nil, 0, ErrMagicNumberWrong
	}

	// 解析CRC、键长度和值长度
	crc := binary.LittleEndian.Uint32(header[4:8])
	keySize := binary.LittleEndian.Uint32(header[8:12])
	valueSize := binary.LittleEndian.Uint32(header[12:16])

	// 读取键数据
	keyData := make([]byte, keySize)
	if _, err = io.ReadFull(r.currentFile, keyData); err != nil {
		log.Println(err)
		return nil, 0, err
	}

	// 读取值数据
	valueData := make([]byte, valueSize)
	if _, err = io.ReadFull(r.currentFile, valueData); err != nil {
		log.Println(err)
		return nil, 0, err
	}

	// 验证CRC校验值（仅对键和值数据计算）
	dataForCRC := append(keyData, valueData...)
	computedCRC := crc32.ChecksumIEEE(dataForCRC)
	if computedCRC != crc {
		log.Println("Checksum mismatch")
		return nil, 0, ErrChecksumMismatch
	}

	// 获取当前读取位置
	currentPos, _ := r.currentFile.Seek(0, io.SeekCurrent)

	return &Entry{
		Key:  keyData,
		Data: valueData,
	}, currentPos, nil
}

// advanceToNextFile 切换到下一个日志文件
// 如果当前是最后一个文件，返回ErrBlocked错误
func (r *Reader) advanceToNextFile() error {
	// 关闭当前文件
	if r.currentFile != nil {
		filename := r.currentFile.Name()
		log.Println("Closing file:", filename)
		_ = r.currentFile.Close()
		r.currentFile = nil

		err := os.Remove(filename)
		if err != nil {
			log.Println(err)
		}
	}

	// 获取所有日志文件列表
	files, err := listVLogFiles(r.vlog.dataDir)
	if err != nil {
		return err
	}

	if len(files) == 0 {
		log.Println("No files found in data directory")
		return ErrFileNotFound
	}

	// 查找当前文件在列表中的位置
	currentIDStr := fmt.Sprintf("%08d%s", r.currentFileID, fileExt)
	currentIdx := sort.SearchStrings(files, currentIDStr)

	// 验证文件是否存在于列表中
	if currentIdx >= len(files) || files[currentIdx] != currentIDStr {
		// 当前文件不在列表中
		// 尝试找到第一个大于当前ID的文件
		if currentIdx >= len(files) {
			// 没有更大的文件
			return ErrBlocked
		}
		// currentIdx 是第一个大于 currentIDStr 的文件位置
		nextFile := files[currentIdx]
		nextFileID := parseFileID(nextFile)

		// 构建下一个文件的路径并打开
		filename := fmt.Sprintf("%08d%s", nextFileID, fileExt)
		path := filepath.Join(r.vlog.dataDir, filename)

		f, err := os.Open(path)
		if err != nil {
			log.Println(err)
			return err
		}

		// 更新读取器状态
		r.currentFile = f
		r.currentFileID = nextFileID
		r.readOffset = 0
		r.checkpoint.CurrentFileID = nextFileID
		r.checkpoint.ReadOffset = 0
		return nil
	}

	// 如果已经是最后一个文件，返回阻塞错误
	if currentIdx >= len(files)-1 {
		return ErrBlocked
	}

	// 获取下一个文件
	nextFile := files[currentIdx+1]
	nextFileID := parseFileID(nextFile)

	// 构建下一个文件的路径并打开
	filename := fmt.Sprintf("%08d%s", nextFileID, fileExt)
	path := filepath.Join(r.vlog.dataDir, filename)

	f, err := os.Open(path)
	if err != nil {
		log.Println(err)
		return err
	}

	// 更新读取器状态
	r.currentFile = f
	r.currentFileID = nextFileID
	r.readOffset = 0
	r.checkpoint.CurrentFileID = nextFileID
	r.checkpoint.ReadOffset = 0

	return nil
}

type ReaderState struct {
	CurrentFileID uint64 // 当前文件的ID
	ReadOffset    int64  // 当前读取位置
}

func (r *Reader) Stat() ReaderState {
	return ReaderState{
		CurrentFileID: r.currentFileID,
		ReadOffset:    r.readOffset,
	}
}

// UpdateCheckpoint 更新并保存检查点
// 将当前读取位置持久化到磁盘
func (r *Reader) UpdateCheckpoint() error {
	r.checkpoint.CurrentFileID = r.currentFileID
	r.checkpoint.ReadOffset = r.readOffset
	return r.checkpoint.Save(r.vlog.dataDir)
}

// DeleteConsumedFiles 删除已经完全消费的日志文件
// 删除当前文件之前的所有文件
func (r *Reader) DeleteConsumedFiles() error {
	files, err := listVLogFiles(r.vlog.dataDir)
	if err != nil {
		log.Println(err)
		return err
	}

	// 查找当前文件在列表中的位置
	currentIDStr := fmt.Sprintf("%08d%s", r.currentFileID, fileExt)
	currentIdx := sort.SearchStrings(files, currentIDStr)

	// 验证文件是否存在于列表中
	if currentIdx >= len(files) || files[currentIdx] != currentIDStr {
		return nil
	}

	// 删除当前文件之前的所有文件
	for i := 0; i < currentIdx; i++ {
		path := filepath.Join(r.vlog.dataDir, files[i])
		if err := os.Remove(path); err != nil {
			log.Println(err)
			return err
		}
	}

	return nil
}

// Close 关闭读取器，释放资源
func (r *Reader) Close() error {
	if r.currentFile != nil {
		return r.currentFile.Close()
	}
	return nil
}

// listVLogFiles 获取数据目录中所有日志文件列表
// 返回按文件名排序的文件列表
func listVLogFiles(dataDir string) ([]string, error) {
	var files []string

	// 遍历目录，收集所有.vlog文件
	err := filepath.Walk(dataDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(info.Name(), fileExt) {
			files = append(files, info.Name())
		}
		return nil
	})

	if err != nil {
		log.Println(err)
		return nil, err
	}

	// 按文件名排序
	sort.Strings(files)
	return files, nil
}

// parseFileID 从文件名中解析文件ID
// 文件名格式：00000001.vlog
func parseFileID(filename string) uint64 {
	// 移除扩展名
	base := strings.TrimSuffix(filename, fileExt)
	// 解析数字部分
	id, _ := strconv.ParseUint(base, 10, 64)
	return id
}
