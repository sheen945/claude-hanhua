package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	_ "embed"
)

//go:embed zh.json.gz
var zhGz []byte

const (
	targetEntry = "i18n/ja-JP.json"
	toolName    = "Claude 桌面版一键汉化"
	toolVer     = "1.0.0"
)

func init() {
	k := syscall.NewLazyDLL("kernel32.dll")
	k.NewProc("SetConsoleOutputCP").Call(65001)
}

type Integrity struct {
	Algorithm string   `json:"algorithm"`
	Hash      string   `json:"hash"`
	BlockSize int64    `json:"blockSize"`
	Blocks    []string `json:"blocks"`
}

type Entry struct {
	Size      int64             `json:"size"`
	Offset    string            `json:"offset"`
	Integrity *Integrity        `json:"integrity"`
	Files     map[string]*Entry `json:"files"`
}

func main() {
	var (
		dir      = flag.String("dir", "", "Claude 安装目录（默认自动检测）")
		restore  = flag.Bool("restore", false, "从备份还原成原版")
		dry      = flag.Bool("dry", false, "只检查不写入")
		list     = flag.Bool("list", false, "列出检测到的 Claude 安装")
		noBackup = flag.Bool("no-backup", false, "不创建备份（不建议）")
	)
	flag.Usage = func() {
		fmt.Printf("%s v%s\n\n", toolName, toolVer)
		fmt.Println("用法: claude-hanhua.exe [参数]")
		fmt.Println()
		fmt.Println("  （无参数）        自动检测并汉化")
		fmt.Println("  -dir <路径>      指定 Claude 安装目录")
		fmt.Println("  -restore         还原成原版（从备份）")
		fmt.Println("  -dry             只检查，不写文件")
		fmt.Println("  -list            列出检测到的安装")
		fmt.Println("  -no-backup       不备份（不建议）")
		fmt.Println()
	}
	flag.Parse()

	fmt.Printf("== %s v%s ==\n\n", toolName, toolVer)

	dirs := []string{}
	if *dir != "" {
		dirs = []string{*dir}
	} else {
		dirs = detectDirs()
	}
	if len(dirs) == 0 {
		fatal("没有找到 Claude 桌面版安装目录。\n" +
			"默认位置: %LOCALAPPDATA%\\AnthropicClaude\\app-<版本>\\\n" +
			"如果装在别处，用 -dir 指定。")
	}

	if *list {
		fmt.Println("检测到的安装：")
		for i, d := range dirs {
			ver := filepath.Base(d)
			ok := "未汉化"
			if hasBackup(d) {
				ok = "已汉化（有备份）"
			}
			fmt.Printf("  [%d] %s  (%s)  %s\n", i+1, d, ver, ok)
		}
		return
	}

	if *restore {
		for _, d := range dirs {
			if err := doRestore(d); err != nil {
				fmt.Printf("[失败] %s: %v\n", d, err)
			}
		}
		return
	}

	for _, d := range dirs {
		fmt.Printf("目标: %s\n", d)
		if err := doPatch(d, *dry, *noBackup); err != nil {
			fmt.Printf("[失败] %v\n", err)
		}
		fmt.Println()
	}
}

func fatal(msg string) {
	fmt.Printf("[错误] %s\n", msg)
	os.Exit(1)
}

func detectDirs() []string {
	base := filepath.Join(os.Getenv("LOCALAPPDATA"), "AnthropicClaude")
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "app-") {
			continue
		}
		d := filepath.Join(base, e.Name())
		if _, err := os.Stat(filepath.Join(d, "resources", "ion-dist.asar")); err == nil {
			out = append(out, d)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}

func asarPath(d string) string { return filepath.Join(d, "resources", "ion-dist.asar") }
func exePath(d string) string  { return filepath.Join(d, "claude.exe") }

func hasBackup(d string) bool {
	_, err := os.Stat(asarPath(d) + ".hanhua-bak")
	return err == nil
}

func loadCatalog() (map[string]string, error) {
	r, err := gzip.NewReader(bytes.NewReader(zhGz))
	if err != nil {
		return nil, fmt.Errorf("解压词库失败: %w", err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("读取词库失败: %w", err)
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("解析词库失败: %w", err)
	}
	return m, nil
}

func findEntry(e *Entry, prefix, target string) *Entry {
	if e == nil || e.Files == nil {
		return nil
	}
	for name, child := range e.Files {
		p := prefix + name
		if child.Files != nil {
			if r := findEntry(child, p+"/", target); r != nil {
				return r
			}
		} else if p == target {
			return child
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func doRestore(d string) error {
	bak := asarPath(d) + ".hanhua-bak"
	if _, err := os.Stat(bak); err != nil {
		return errors.New("没有找到备份，无法还原")
	}
	exeBak := exePath(d) + ".hanhua-bak"
	if err := os.Rename(bak, asarPath(d)); err != nil {
		if err := copyFile(bak, asarPath(d)); err != nil {
			return err
		}
		os.Remove(bak)
	}
	if _, err := os.Stat(exeBak); err == nil {
		if err := os.Rename(exeBak, exePath(d)); err != nil {
			if err := copyFile(exeBak, exePath(d)); err != nil {
				return err
			}
			os.Remove(exeBak)
		}
	}
	fmt.Printf("[完成] 已还原: %s\n", d)
	return nil
}

func doPatch(d string, dry, noBackup bool) error {
	ap, ep := asarPath(d), exePath(d)
	if _, err := os.Stat(ap); err != nil {
		return fmt.Errorf("找不到 %s", ap)
	}
	if _, err := os.Stat(ep); err != nil {
		return fmt.Errorf("找不到 %s", ep)
	}

	zh, err := loadCatalog()
	if err != nil {
		return err
	}
	fmt.Printf("  内置词库: %d 条\n", len(zh))

	buf, err := os.ReadFile(ap)
	if err != nil {
		return fmt.Errorf("读取 asar 失败（Claude 可能正在运行，先退出）: %w", err)
	}
	if len(buf) < 16 {
		return errors.New("asar 文件损坏")
	}
	// asar 头部布局: [0:4]=4, [4:8]=headerSize(pickle 总长), [8:12]=headerSize-4,
	// [12:16]=header JSON 字节长度, 之后是 JSON 本体, 然后是 padding 到 8+headerSize
	headerSize := int64(binary.LittleEndian.Uint32(buf[4:8]))
	jsonLen := int64(binary.LittleEndian.Uint32(buf[12:16]))
	dataStart := 8 + headerSize
	headerEnd := 16 + jsonLen
	if headerEnd > int64(len(buf)) {
		return errors.New("asar 头部长度异常")
	}
	headerRaw := string(buf[16:headerEnd])

	var root Entry
	if err := json.Unmarshal([]byte(headerRaw), &root); err != nil {
		return fmt.Errorf("解析 asar 头部失败: %w", err)
	}
	ent := findEntry(&root, "", targetEntry)
	if ent == nil {
		return fmt.Errorf("asar 里找不到 %s，可能这个版本结构不同", targetEntry)
	}
	if ent.Integrity == nil {
		return errors.New("该条目没有完整性信息，格式不支持")
	}
	off, err := strconv.ParseInt(ent.Offset, 10, 64)
	if err != nil {
		return fmt.Errorf("offset 解析失败: %w", err)
	}
	abs := dataStart + off
	if abs+ent.Size > int64(len(buf)) {
		return errors.New("条目位置越界")
	}
	orig := buf[abs : abs+ent.Size]
	trimmed := bytes.TrimRight(orig, " \t\r\n\x00")

	var ja map[string]string
	if err := json.Unmarshal(trimmed, &ja); err != nil {
		return fmt.Errorf("解析目标语言表失败: %w", err)
	}
	fmt.Printf("  目标语言表: %d 条，槽位 %d 字节\n", len(ja), ent.Size)

	merged := make(map[string]string, len(ja))
	var hit, miss int
	for k, v := range ja {
		if nv, ok := zh[k]; ok && strings.TrimSpace(nv) != "" {
			merged[k] = nv
			hit++
		} else {
			merged[k] = v
			miss++
		}
	}
	fmt.Printf("  命中中文: %d 条，保留原文: %d 条（%.1f%% 汉化）\n",
		hit, miss, float64(hit)/float64(len(ja))*100)

	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(merged); err != nil {
		return fmt.Errorf("生成新语言表失败: %w", err)
	}
	outBytes := bytes.TrimRight(body.Bytes(), "\n")
	if int64(len(outBytes)) > ent.Size {
		return fmt.Errorf("新语言表 %d 字节，超出槽位 %d 字节", len(outBytes), ent.Size)
	}
	filled := make([]byte, ent.Size)
	copy(filled, outBytes)
	for i := len(outBytes); i < len(filled); i++ {
		filled[i] = 0x20
	}

	if dry {
		fmt.Println("  [dry-run] 校验通过，未写入")
		return nil
	}

	if !noBackup && !hasBackup(d) {
		fmt.Println("  备份原文件...")
		if err := copyFile(ap, ap+".hanhua-bak"); err != nil {
			return fmt.Errorf("备份 asar 失败: %w", err)
		}
		if err := copyFile(ep, ep+".hanhua-bak"); err != nil {
			return fmt.Errorf("备份 exe 失败: %w", err)
		}
	}

	oldHash := ent.Integrity.Hash
	oldBlocks := append([]string{}, ent.Integrity.Blocks...)
	sum := sha256.Sum256([]byte(headerRaw))
	oldHeaderHash := hex.EncodeToString(sum[:])

	copy(buf[abs:abs+ent.Size], filled)
	if err := os.WriteFile(ap, buf, 0644); err != nil {
		return fmt.Errorf("写入 asar 失败: %w", err)
	}

	newSum := sha256.Sum256(filled)
	newHash := hex.EncodeToString(newSum[:])
	var newBlocks []string
	bs := ent.Integrity.BlockSize
	if bs <= 0 {
		bs = 4194304
	}
	for i := int64(0); i < int64(len(filled)); i += bs {
		end := i + bs
		if end > int64(len(filled)) {
			end = int64(len(filled))
		}
		s := sha256.Sum256(filled[i:end])
		newBlocks = append(newBlocks, hex.EncodeToString(s[:]))
	}

	if strings.Count(headerRaw, oldHash) != 1 {
		return errors.New("条目哈希在头部不唯一，放弃（请反馈这个情况）")
	}
	newHeader := strings.Replace(headerRaw, oldHash, newHash, 1)
	for i := range oldBlocks {
		if i >= len(newBlocks) {
			break
		}
		if strings.Count(newHeader, oldBlocks[i]) != 1 {
			return errors.New("分块哈希在头部不唯一，放弃")
		}
		newHeader = strings.Replace(newHeader, oldBlocks[i], newBlocks[i], 1)
	}
	if len(newHeader) != len(headerRaw) {
		return errors.New("头部长度变化，放弃")
	}
	copy(buf[16:headerEnd], []byte(newHeader))
	if err := os.WriteFile(ap, buf, 0644); err != nil {
		return fmt.Errorf("写入头部失败: %w", err)
	}

	exeBuf, err := os.ReadFile(ep)
	if err != nil {
		return fmt.Errorf("读取 exe 失败: %w", err)
	}
	pos := bytes.Index(exeBuf, []byte(oldHeaderHash))
	if pos < 0 {
		return errors.New("exe 里找不到 asar 头部哈希（版本结构不同，请反馈）")
	}
	if bytes.Count(exeBuf, []byte(oldHeaderHash)) != 1 {
		return errors.New("exe 里哈希不唯一，放弃")
	}
	finalSum := sha256.Sum256([]byte(newHeader))
	finalHash := hex.EncodeToString(finalSum[:])
	copy(exeBuf[pos:pos+64], []byte(finalHash))
	if err := os.WriteFile(ep, exeBuf, 0644); err != nil {
		return fmt.Errorf("写入 exe 失败: %w", err)
	}

	fmt.Println("  [完成] 汉化成功，重新打开 Claude 即可")
	fmt.Printf("  备份: %s.hanhua-bak\n", ap)
	return nil
}
