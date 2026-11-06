package webdav

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"oneimg/backend/utils/storageobject"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	BaseURL  string
	Username string
	Password string
	Timeout  time.Duration
}

func (c *WebDAVClient) objectURL(path string) string {
	u, err := url.Parse(c.config.BaseURL)
	if err != nil {
		return ""
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + c.NormalizePath(path)
	u.RawPath = ""
	return u.String()
}

type WebDAVClient struct {
	config Config
}

// 创建WebDAV客户端
func Client(cfg Config) *WebDAVClient {
	// 设置超时时间
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}

	// 确保BaseURL以"/"结尾
	if cfg.BaseURL != "" && !strings.HasSuffix(cfg.BaseURL, "/") {
		cfg.BaseURL += "/"
	}
	return &WebDAVClient{config: cfg}
}

func (c *WebDAVClient) NormalizePath(path string) string {
	// 空路径直接返回空，避免转为.
	if path == "" || path == "." {
		return ""
	}
	// 替换所有反斜杠为正斜杠
	path = strings.ReplaceAll(path, "\\", "/")
	// 去除重复的 /
	parts := strings.Split(path, "/")
	cleanParts := []string{}
	for _, part := range parts {
		if part != "" && part != "." { // 过滤.和空字符串
			cleanParts = append(cleanParts, part)
		}
	}
	// 重新拼接为标准路径
	cleanPath := strings.Join(cleanParts, "/")
	return cleanPath
}

// WebDAVStat 检查路径是否存在
func (c *WebDAVClient) WebDAVStat(ctx context.Context, path string) (bool, error) {
	// 标准化路径
	cleanPath := c.NormalizePath(path)
	fullURL := c.objectURL(cleanPath)

	req, err := http.NewRequestWithContext(ctx, "PROPFIND", fullURL, nil)
	if err != nil {
		return false, fmt.Errorf("构建请求失败：%w", err)
	}

	if c.config.Username != "" && c.config.Password != "" {
		req.SetBasicAuth(c.config.Username, c.config.Password)
	}
	req.Header.Set("Depth", "0")
	req.Header.Set("User-Agent", "OneIMG/3.0")

	client := &http.Client{Timeout: c.config.Timeout}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("请求失败：%w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case 207:
		return true, nil
	case 404:
		return false, nil
	case 401:
		return false, errors.New("认证失败：用户名或密码错误")
	case 403:
		return false, errors.New("权限不足：无访问该路径的权限")
	case 500:
		return false, fmt.Errorf("服务器内部错误（可能是路径格式错误）：%s", cleanPath)
	default:
		return false, fmt.Errorf("未知错误，状态码：%d", resp.StatusCode)
	}
}

// WebDAVMkdirAll 递归创建目录
func (c *WebDAVClient) WebDAVMkdirAll(ctx context.Context, dirPath string) error {
	// 标准化路径
	cleanDir := c.NormalizePath(dirPath)
	if cleanDir == "" {
		return nil
	}

	// 拆分路径为多级
	parts := strings.Split(cleanDir, "/")
	currentPath := ""
	for _, part := range parts {
		if part == "" {
			continue
		}
		// 拼接当前级路径（始终用 / 分隔）
		currentPath += part + "/"

		// 检查当前目录是否存在
		exists, err := c.WebDAVStat(ctx, currentPath)
		if err != nil {
			return fmt.Errorf("检查目录 %s 失败：%w", currentPath, err)
		}
		if exists {
			continue
		}

		// 构建 MKCOL 请求（创建目录）
		fullURL := c.objectURL(currentPath)
		req, err := http.NewRequestWithContext(ctx, "MKCOL", fullURL, nil)
		if err != nil {
			return fmt.Errorf("构建创建目录请求失败：%w", err)
		}
		if c.config.Username != "" && c.config.Password != "" {
			req.SetBasicAuth(c.config.Username, c.config.Password)
		}

		client := &http.Client{Timeout: c.config.Timeout}
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("创建目录 %s 失败：%w", currentPath, err)
		}
		defer resp.Body.Close()

		log.Printf("创建目录 %s 响应状态码：%d", currentPath, resp.StatusCode)

		// 兼容更多状态码：201=创建成功，405=已存在，204=无内容（部分服务器返回）
		if resp.StatusCode != 201 && resp.StatusCode != 405 && resp.StatusCode != 204 {
			return fmt.Errorf("创建目录 %s 失败，状态码：%d", currentPath, resp.StatusCode)
		}
		log.Printf("目录 %s 创建成功", currentPath)
	}
	return nil
}

// WebDAVUpload 上传文件（入参为io.Reader，兼容所有流类型）
func (c *WebDAVClient) WebDAVUpload(ctx context.Context, remotePath string, file io.Reader) error {
	// 标准化远程路径
	cleanRemotePath := c.NormalizePath(remotePath)
	dirPath := filepath.Dir(cleanRemotePath)
	if dirPath == "." {
		dirPath = ""
	}

	// 创建目录
	if dirPath != "" {
		if err := c.WebDAVMkdirAll(ctx, dirPath); err != nil {
			return fmt.Errorf("创建文件目录失败：%w", err)
		}
	}

	// 读取文件内容到字节数组
	fileBytes, err := io.ReadAll(file)
	if err != nil {
		return fmt.Errorf("读取上传文件内容失败：%w", err)
	}
	fileSize := int64(len(fileBytes))
	if fileSize == 0 {
		return errors.New("上传文件内容为空")
	}

	// 构建上传URL
	fullURL := c.objectURL(cleanRemotePath)
	req, err := http.NewRequestWithContext(ctx, "PUT", fullURL, bytes.NewReader(fileBytes))
	if err != nil {
		return fmt.Errorf("构建上传请求失败：%w", err)
	}

	// 设置认证和请求头
	if c.config.Username != "" && c.config.Password != "" {
		req.SetBasicAuth(c.config.Username, c.config.Password)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Length", strconv.FormatInt(fileSize, 10))
	req.Header.Set("User-Agent", "OneIMG/3.0")

	client := &http.Client{Timeout: c.config.Timeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("上传失败：%w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		log.Printf("文件 %s 上传成功", cleanRemotePath)
		return nil
	}
	return fmt.Errorf("上传失败，状态码：%d", resp.StatusCode)
}

func (c *WebDAVClient) WebDAVCreate(ctx context.Context, path string, body io.Reader) error {
	dir := filepath.Dir(c.NormalizePath(path))
	if dir != "." && dir != "" {
		if err := c.WebDAVMkdirAll(ctx, dir); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.objectURL(path), body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.config.Username, c.config.Password)
	req.Header.Set("If-None-Match", "*")
	client := &http.Client{Timeout: c.config.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return storageobject.ErrOutcomeUncertain
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusCreated:
		return nil
	case http.StatusPreconditionFailed:
		return storageobject.ErrConflict
	case http.StatusOK, http.StatusNoContent:
		return errors.Join(storageobject.ErrConditionalUnsupported, storageobject.ErrOutcomeUncertain)
	default:
		return fmt.Errorf("conditional WebDAV create failed: status %d", resp.StatusCode)
	}
}

// WebDAVDelete 删除文件/目录
func (c *WebDAVClient) WebDAVDelete(ctx context.Context, path string) error {
	cleanPath := c.NormalizePath(path)
	fullURL := c.objectURL(cleanPath)

	req, err := http.NewRequestWithContext(ctx, "DELETE", fullURL, nil)
	if err != nil {
		return fmt.Errorf("构建删除请求失败：%w", err)
	}
	if c.config.Username != "" && c.config.Password != "" {
		req.SetBasicAuth(c.config.Username, c.config.Password)
	}

	client := &http.Client{Timeout: c.config.Timeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("删除失败：%w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("删除失败，状态码：%d", resp.StatusCode)
}

// WebDAVMedia bounds connection/header waits but leaves streaming lifetime to the request context.
func (c *WebDAVClient) WebDAVMedia(ctx context.Context, path, method, byteRange, ifRange string, validators ...string) (*http.Response, error) {
	if method != http.MethodGet && method != http.MethodHead {
		return nil, errors.New("public media is read-only")
	}
	req, err := http.NewRequestWithContext(ctx, method, c.objectURL(path), nil)
	if err != nil {
		return nil, err
	}
	if c.config.Username != "" {
		req.SetBasicAuth(c.config.Username, c.config.Password)
	}
	req.Header.Set("Accept-Encoding", "identity")
	if len(validators) > 0 {
		req.Header.Set("If-None-Match", validators[0])
	}
	if len(validators) > 1 {
		req.Header.Set("If-Modified-Since", validators[1])
	}
	if byteRange != "" {
		req.Header.Set("Range", byteRange)
		if ifRange != "" {
			req.Header.Set("If-Range", ifRange)
		}
	}
	return mediaClient.Do(req)
}

var mediaClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: &http.Transport{
	Proxy:               http.ProxyFromEnvironment,
	DialContext:         (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second, IdleConnTimeout: 90 * time.Second,
}}
