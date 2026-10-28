package baidu

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/jenken827/pfm-plugin-sdk/driver/types"
	"github.com/jenken827/pfm-plugin-sdk/plugin/sdk/logx"
)

// BaiduNetdiskDriver 百度网盘驱动
type BaiduNetdiskDriver struct {
	Config *Config
	// providerID 宿主下发的 provider 主键，仅用于把刷新后的 token 回写配置
	providerID uint32
	client     *http.Client

	ProviderConfig  types.ProviderConfig
	configPersister types.ProviderConfigPersister

	// tokenMu 保护 accessToken / refreshToken：分片上传时多个协程并发读，
	// 续期（oplist / 百度官方）时写
	tokenMu sync.RWMutex
	// refreshMu 串行化续期：两条续期路径都会轮换 refresh_token，
	// 并发续期会让彼此刚拿到的新值立刻作废
	refreshMu sync.Mutex

	accessToken    string
	refreshToken   string
	vipType        int
	customPartSize int64
}

// GetType 获取存储驱动类型
func (d *BaiduNetdiskDriver) GetType() string {
	return DriverID
}

// GetName 获取驱动名称
func (d *BaiduNetdiskDriver) GetName() string {
	return DriverID
}

// GetConfig 获取驱动配置
func (d *BaiduNetdiskDriver) GetConfig() any {
	return d.Config
}

// GetProviderConfig 获取存储提供者的类型化配置视图
func (d *BaiduNetdiskDriver) GetProviderConfig() *types.ProviderConfig {
	return &d.ProviderConfig
}

// SetConfigPersister 注入配置持久化器（用于 token 刷新后持久化）
func (d *BaiduNetdiskDriver) SetConfigPersister(p types.ProviderConfigPersister) {
	d.configPersister = p
}

// baiduDirBackend 基于百度网盘 API 的目录后端
type baiduDirBackend struct{ d *BaiduNetdiskDriver }

func (b *baiduDirBackend) List(ctx context.Context, dir string) ([]types.DirEntry, error) {
	files, err := b.d.getFileList(dir)
	if err != nil {
		return nil, err
	}
	result := make([]types.DirEntry, 0, len(files))
	for _, f := range files {
		result = append(result, types.DirEntry{Name: f.ServerFilename, IsDir: f.Isdir == 1})
	}
	return result, nil
}

func (b *baiduDirBackend) Mkdir(ctx context.Context, dir string) error {
	return b.d.mkdir(dir)
}

// rootPath 获取根目录路径（去除尾部斜杠）
//
// 设计：rootPath 用于 path.Join 拼接上传子路径，必须**始终以 "/" 开头**，
// 否则 path.Join("/", "2026", "08") 会得到 "/2026/08" 但 path.Join("", "2026", "08")
// 会得到 "2026/08"（失去 / 前缀），导致 file_locations.path 字段在重构前后不一致。
// 见 commit 3966af0 (2026-08-17) 引入的 path 拼接变更及后续无 / 记录问题。
func (d *BaiduNetdiskDriver) rootPath() string {
	rootPath := "/"
	if d.Config != nil && d.Config.RootPath != "" {
		rootPath = d.Config.RootPath
	}
	// 去除尾部斜杠但**保留前导斜杠**，保证 path.Join(rootPath, "2026", "08") 形如 "/2026/08"
	rootPath = strings.TrimSuffix(rootPath, "/")
	if !strings.HasPrefix(rootPath, "/") {
		rootPath = "/" + rootPath
	}
	return rootPath
}

// Upload 上传文件
func (d *BaiduNetdiskDriver) Upload(ctx context.Context, src types.UploadSource, opt types.UploadOptions) ([]types.FileRef, error) {
	fileName := src.FileName

	// 百度网盘不允许上传空文件
	if src.FileSize < 1 {
		return nil, fmt.Errorf("baidu netdisk does not allow empty files")
	}

	// 1. 准备上传内容（加密管道共享逻辑）：加密则写临时文件并混淆文件名
	uploadFilePath, uploadName, encSize, err := types.PackToTempFile(src.TempPath, fileName, opt)
	if err != nil {
		return nil, err
	}
	if uploadFilePath != src.TempPath {
		defer os.Remove(uploadFilePath) // 加密临时文件，上传后清理
	}
	fileSize := int64(encSize)

	// 2. 确保上传目录存在（年/月结构）
	uploadDir, err := types.AllocateUploadDir(ctx, d.rootPath(), &baiduDirBackend{d: d})
	if err != nil {
		return nil, fmt.Errorf("ensure upload directory failed: %w", err)
	}
	fullPath := uploadDir + "/" + uploadName

	// 3. 打开文件
	file, err := os.Open(uploadFilePath)
	if err != nil {
		return nil, fmt.Errorf("open file failed: %w", err)
	}
	defer file.Close()

	// 4. 计算 MD5（provider 特有 hash，留在驱动内）
	md5Hash := md5.New()
	if _, err := io.Copy(md5Hash, file); err != nil {
		return nil, fmt.Errorf("calculate md5 failed: %w", err)
	}
	contentMD5 := hex.EncodeToString(md5Hash.Sum(nil))
	file.Seek(0, io.SeekStart)

	// 5. 尝试秒传（通过 create 接口）
	createResp, err := d.create(fullPath, fileSize, 0, "", "", 0, 0)
	if err == nil && createResp.FsId > 0 {
		logx.C(ctx).Infof("baidu netdisk rapid upload success: %s", uploadName)
		return []types.FileRef{{
			ProviderFileID: strconv.FormatInt(createResp.FsId, 10),
			Name:           uploadName,
			Path:           fullPath,
			Size:           uint64(fileSize),
			ChunkSize:      uint64(fileSize),
			Hash:           contentMD5,
		}}, nil
	}

	// 6. 统一使用分片上传接口（/xpan/file），小文件作为单个分片上传
	return d.uploadChunkedFile(ctx, file, fullPath, uploadName, fileSize, contentMD5)
}

// uploadChunkedFile 分片上传大文件
func (d *BaiduNetdiskDriver) uploadChunkedFile(ctx context.Context, file *os.File, fullPath, fileName string, fileSize int64, contentMD5 string) ([]types.FileRef, error) {
	logx.C(ctx).Infof("baidu netdisk chunked upload: %s, size: %d", fileName, fileSize)

	sliceSize := d.getSliceSize(fileSize)
	count := int((fileSize + sliceSize - 1) / sliceSize)
	lastBlockSize := fileSize % sliceSize
	if lastBlockSize == 0 {
		lastBlockSize = sliceSize
	}

	// 计算每个分片的 MD5
	blockList := make([]string, 0, count)
	sliceMD5Hash := md5.New()
	sliceMD5Hash256k := md5.New()
	buf := make([]byte, sliceSize)

	for i := 0; i < count; i++ {
		readSize := sliceSize
		if i == count-1 {
			readSize = lastBlockSize
		}

		n, err := io.ReadFull(file, buf[:readSize])
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("read file failed: %w", err)
		}

		sliceMD5Hash.Reset()
		sliceMD5Hash.Write(buf[:n])
		blockList = append(blockList, hex.EncodeToString(sliceMD5Hash.Sum(nil)))

		// 计算前 256KB 的 MD5（用于秒传）
		if i == 0 {
			sliceSize256k := int64(256 * 1024)
			if int64(n) > sliceSize256k {
				sliceMD5Hash256k.Reset()
				sliceMD5Hash256k.Write(buf[:sliceSize256k])
			}
		}
	}

	blockListJSON, _ := json.Marshal(blockList)
	sliceMD5 := hex.EncodeToString(sliceMD5Hash256k.Sum(nil))

	// 预上传
	file.Seek(0, io.SeekStart)
	precreateResp, err := d.precreate(fullPath, fileSize, string(blockListJSON), contentMD5, sliceMD5)
	if err != nil {
		return nil, fmt.Errorf("precreate failed: %w", err)
	}

	// 秒传成功
	if precreateResp.ReturnType == 2 {
		logx.C(ctx).Infof("baidu netdisk rapid upload success (precreate): %s", fileName)
		return []types.FileRef{{
			ProviderFileID: strconv.FormatInt(precreateResp.File.FsId, 10),
			Name:           fileName,
			Path:           fullPath,
			Size:           uint64(fileSize),
			ChunkSize:      uint64(fileSize),
			Hash:           contentMD5,
			ChunkHash:      blockList[0],
		}}, nil
	}

	// 获取并发数配置
	uploadThread := 3 // 默认 3 个并发
	if t := int(d.Config.UploadThread.Int()); t > 0 && t <= 32 {
		uploadThread = t
	}
	logx.C(ctx).Infof("baidu netdisk upload thread: %d", uploadThread)

	// 分片上传（并发）
	uploadURL := d.getUploadURL(fullPath, precreateResp.Uploadid)
	uploadParams := map[string]string{
		"method":       "upload",
		"access_token": d.token(),
		"type":         "tmpfile",
		"path":         fullPath,
		"uploadid":     precreateResp.Uploadid,
	}

	// 使用信号量控制并发
	sem := make(chan struct{}, uploadThread)
	var wg sync.WaitGroup
	errChan := make(chan error, len(precreateResp.BlockList))

	for i, partseq := range precreateResp.BlockList {
		if partseq < 0 {
			continue
		}

		offset := int64(partseq) * sliceSize
		size := sliceSize
		if partseq+1 == count {
			size = lastBlockSize
		}

		sectionReader := io.NewSectionReader(file, offset, size)
		currentPartseq := partseq
		currentIndex := i

		wg.Add(1)
		go func(partseq int, index int, reader *io.SectionReader, sliceSize int64) {
			defer wg.Done()

			// 获取信号量
			sem <- struct{}{}
			defer func() { <-sem }()

			params := make(map[string]string)
			for k, v := range uploadParams {
				params[k] = v
			}
			params["partseq"] = strconv.Itoa(partseq)

			if err := d.uploadSlice(uploadURL, params, fileName, reader, sliceSize); err != nil {
				logx.C(ctx).Errorf("baidu netdisk upload slice %d failed: %v", index+1, err)
				errChan <- fmt.Errorf("upload slice %d failed: %w", index, err)
				return
			}

			logx.C(ctx).Debugf("baidu netdisk uploaded slice %d/%d", index+1, len(precreateResp.BlockList))
		}(currentPartseq, currentIndex, sectionReader, size)
	}

	// 等待所有分片上传完成
	wg.Wait()
	close(errChan)

	// 检查是否有错误
	if err := <-errChan; err != nil {
		return nil, err
	}

	// 创建文件
	createResp, err := d.create(fullPath, fileSize, 0, precreateResp.Uploadid, string(blockListJSON), 0, 0)
	if err != nil {
		return nil, fmt.Errorf("create file failed: %w", err)
	}

	logx.C(ctx).Infof("baidu netdisk chunked upload success: %s, fs_id: %d", fileName, createResp.FsId)

	return []types.FileRef{{
		ProviderFileID: strconv.FormatInt(createResp.FsId, 10),
		Name:           fileName,
		Path:           fullPath,
		Size:           uint64(fileSize),
		ChunkSize:      uint64(fileSize),
		Hash:           contentMD5,
		ChunkHash:      blockList[0],
	}}, nil
}

// Open 打开文件（支持多分片合并）
func (d *BaiduNetdiskDriver) Open(ctx context.Context, refs []types.FileRef, opt types.OpenOptions) (types.ReadSeekCloser, error) {
	if len(refs) == 0 {
		return nil, fmt.Errorf("no file refs provided")
	}

	// 按分片编号排序，创建多分片合并 Reader（支持 Seek 与 Range 请求）
	sort.Slice(refs, func(i, j int) bool {
		return refs[i].ChunkNumber < refs[j].ChunkNumber
	})

	return types.NewMultiChunkReader(ctx, d, refs, opt.Encryptor)
}

// OpenChunk 实现 types.ChunkOpener 接口
// 下载指定分片，支持从指定偏移位置开始（通过 HTTP Range 请求，避免全量下载后丢弃）
// 注意：对于加密文件，返回的 Reader 从文件头开始，让 Decrypt 处理 seek
func (d *BaiduNetdiskDriver) OpenChunk(ctx context.Context, ref types.FileRef, offset int64) (io.ReadCloser, error) {
	if ref.ProviderFileID == "" {
		return nil, fmt.Errorf("provider_file_id is empty")
	}

	rc, err := d.downloadRange(ctx, ref, offset)
	if err != nil {
		return nil, err
	}

	// 非 Range 请求（offset=0 且服务端未返回 206）时，跳过前面的字节
	if offset > 0 {
		if _, err := io.CopyN(io.Discard, rc, offset); err != nil {
			rc.Close()
			return nil, fmt.Errorf("skip bytes failed: %w", err)
		}
	}

	return rc, nil
}

// OpenChunkRangeSeek 实现 types.ChunkOpener 接口
// 返回一个 OpenRangeSeek 函数，支持 Range 请求和 Seek 操作
// 用于加密文件的解密
func (d *BaiduNetdiskDriver) OpenChunkRangeSeek(ctx context.Context, ref types.FileRef) types.OpenRangeSeek {
	return func(ctx context.Context, offset, limit int64) (io.ReadCloser, error) {
		// 打开文件（从文件头开始，加密文件必须从头解密）
		rc, err := d.OpenChunk(ctx, ref, 0)
		if err != nil {
			return nil, err
		}

		// 如果需要 seek 到指定位置
		if offset > 0 {
			if seeker, ok := rc.(io.Seeker); ok {
				if _, err := seeker.Seek(offset, io.SeekStart); err != nil {
					rc.Close()
					return nil, fmt.Errorf("seek failed: %w", err)
				}
			} else {
				// 如果不支持 Seek，跳过前面的字节
				if _, err := io.CopyN(io.Discard, rc, offset); err != nil {
					rc.Close()
					return nil, fmt.Errorf("skip bytes failed: %w", err)
				}
			}
		}

		// 限制读取长度（limit>0 时）
		if limit > 0 {
			return io.NopCloser(io.LimitReader(rc, limit)), nil
		}

		return rc, nil
	}
}

// downloadRange 通过 HTTP Range 请求下载文件的一部分
// offset=0 且 limit=0 时下载完整文件
func (d *BaiduNetdiskDriver) downloadRange(ctx context.Context, ref types.FileRef, offset int64) (io.ReadCloser, error) {
	// 获取下载链接
	fsIDStr := ref.ProviderFileID
	fsID, err := strconv.ParseInt(fsIDStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid fs_id: %w", err)
	}

	downloadURL, err := d.getDownloadLink(fsID)
	if err != nil {
		return nil, fmt.Errorf("get download link failed: %w", err)
	}

	// 下载文件
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "pan.baidu.com")

	// 携带 Range 头，只下载需要的部分（避免限速场景下全量下载）
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		resp.Body.Close()
		return nil, fmt.Errorf("download failed: status=%d", resp.StatusCode)
	}

	return resp.Body, nil
}

// BatchDelete 批量删除文件
//
// 错误码语义（百度官方）：
//   - errno=12 "批量转存出错"：批量调用整体失败
//   - info[].errno=-7 "文件或目录名错误或无权访问"：百度侧路径无效
//
// 这两类错误都意味着"百度侧根本找不到这个路径"，对清理孤儿场景等价于"已删除"。
// 当全部失败项的内层 errno 都是 -7 时，本函数把结果标记为 Success=true，让上层
// 直接清掉数据库行；否则维持原行为，失败项供上层标 DELETED_FAILED。
func (d *BaiduNetdiskDriver) BatchDelete(ctx context.Context, refs []types.FileRef) ([]types.DeleteResult, error) {
	if len(refs) == 0 {
		return nil, nil
	}

	logx.C(ctx).Infof("baidu netdisk batch delete: %d files", len(refs))

	deleteList := make([]map[string]interface{}, 0, len(refs))
	for _, ref := range refs {
		if ref.ProviderFileID == "" {
			continue
		}
		// 百度网盘使用 path 删除文件
		if ref.Path == "" {
			continue
		}
		deleteList = append(deleteList, map[string]interface{}{
			"path": ref.Path,
		})
	}

	if len(deleteList) == 0 {
		return nil, nil
	}

	// 调用百度网盘 API 删除文件
	err := d.manage("delete", deleteList)
	if err == nil {
		// 全部成功
		results := make([]types.DeleteResult, len(refs))
		for i, ref := range refs {
			results[i] = types.DeleteResult{
				ID:      ref.LocationID,
				Success: true,
			}
		}
		logx.C(ctx).Infof("baidu netdisk batch delete success: %d files", len(deleteList))
		return results, nil
	}

	// 全部失败：若所有项的内层 errno 都是 -7（路径无效），视为"百度侧不可达 = 已清理"
	if allItemsPathNotFound(err) {
		logx.C(ctx).Infof("baidu netdisk batch delete: all %d items path-not-found on baidu (errno=-7), treating as cleaned",
			len(deleteList))
		results := make([]types.DeleteResult, len(refs))
		for i, ref := range refs {
			results[i] = types.DeleteResult{
				ID:      ref.LocationID,
				Success: true,
			}
		}
		return results, nil
	}

	// 其他失败：保留原行为，让上层标 DELETED_FAILED。
	// 参数错误（errno=12）这类重试也不会成功，标记 Permanent，宿主据此只记 warning——
	// 这类平台语义由驱动自己判断，宿主不匹配错误文案。
	logx.C(ctx).Errorf("baidu netdisk batch delete failed: %v", err)
	permanent := topLevelErrno(err) == 12
	results := make([]types.DeleteResult, len(refs))
	for i, ref := range refs {
		results[i] = types.DeleteResult{
			ID:        ref.LocationID,
			Success:   false,
			Error:     err,
			Permanent: permanent,
		}
	}
	return results, nil
}

// topLevelErrno 取百度 manage 接口返回的最外层 errno（解析不出返回 0）
func topLevelErrno(err error) int {
	if err == nil {
		return 0
	}
	body := err.Error()
	start := strings.Index(body, "{")
	end := strings.LastIndex(body, "}")
	if start < 0 || end <= start {
		return 0
	}
	var parsed struct {
		Errno int `json:"errno"`
	}
	if jerr := json.Unmarshal([]byte(body[start:end+1]), &parsed); jerr != nil {
		return 0
	}
	return parsed.Errno
}

// allItemsPathNotFound 解析百度 manage 错误信息，若所有 info[].errno 都是 -7 则返回 true
//
// err 形如 `errno=12 body={"errno":12,"info":[{"errno":-7,...}],...}`。
// 直接从第一个 '{' 到最后一个 '}' 截取 JSON 整体解析——不能用第一个 '}' 截断，
// 否则嵌套 JSON（info 数组内的对象）会被切坏导致解析失败、检测永远失效。
func allItemsPathNotFound(err error) bool {
	if err == nil {
		return false
	}
	body := err.Error()
	start := strings.Index(body, "{")
	end := strings.LastIndex(body, "}")
	if start < 0 || end <= start {
		return false
	}
	body = body[start : end+1]
	var parsed struct {
		Errno int                      `json:"errno"`
		Info  []map[string]interface{} `json:"info"`
	}
	if jerr := json.Unmarshal([]byte(body), &parsed); jerr != nil {
		return false
	}
	if len(parsed.Info) == 0 {
		return false
	}
	for _, item := range parsed.Info {
		e, ok := item["errno"]
		if !ok {
			return false
		}
		// JSON 数字解析为 float64
		if f, ok := e.(float64); !ok || int(f) != -7 {
			return false
		}
	}
	return true
}
