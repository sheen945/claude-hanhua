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
	"time"

	_ "embed"
)

//go:embed zh.json.gz
var zhGz []byte

const (
	targetEntry = "i18n/ja-JP.json"
	toolName    = "Claude 桌面版一键汉化"
	toolVer     = "1.1.0"
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

// State 记录上次汉化时的文件指纹，用来判断 Claude 是否升级过
type State struct {
	ToolVersion string `json:"toolVersion"`
	PatchedAt   string `json:"patchedAt"`
	InstallDir  string `json:"installDir"`
	Version     string `json:"version"`
	HeaderHash  string `json:"asarHeaderHash"`
	AsarSize    int64  `json:"asarSize"`
	AsarMTime   int64  `json:"asarMTimeUnix"`
	Entries     int    `json:"entries"`
	Hit         int    `json:"hit"`
}

type AsarInfo struct {
	Buf       []byte
	HeaderRaw string
	HeaderEnd int64
	DataStart int64
	Entry     *Entry
	Abs       int64
	Content   []byte
	HeaderSHA string
}

type Analysis struct {
	Dir     string
	Version string
	Total   int
	Chinese int
	Ratio   float64
	Info    *AsarInfo
	State   *State
	Verdict string // already / partial / stale / never / newer
}

func main() {
	var (
		dir      = flag.String("dir", "", "Claude 安装目录（默认自动检测）")
		restore  = flag.Bool("restore", false, "从备份还原成原版")
		dry      = flag.Bool("dry", false, "只检查不写入")
		list     = flag.Bool("list", false, "列出检测到的 Claude 安装")
		status   = flag.Bool("status", false, "只看汉化状态与升级判断")
		force    = flag.Bool("force", false, "强制重新汉化（忽略「已是中文」判断）")
		noBackup = flag.Bool("no-backup", false, "不创建备份（不建议）")
	)
	flag.Usage = func() {
		fmt.Printf("%s v%s\n\n", toolName, toolVer)
		fmt.Println("用法: claude-hanhua.exe [参数]")
		fmt.Println()
		fmt.Println("  （无参数）        自动检测：已是中文就跳过，升级过就重打")
		fmt.Println("  -status          只看状态诊断，不改任何文件")
		fmt.Println("  -dir <路径>      指定 Claude 安装目录")
		fmt.Println("  -restore         还原成原版（从备份）")
		fmt.Println("  -dry             只检查，不写文件")
		fmt.Println("  -force           强制重新汉化")
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

	st := loadState()
	warnNewerVersion(dirs, st)

	if *list {
		fmt.Println("检测到的安装：")
		for i, d := range dirs {
			fmt.Printf("  [%d] %s\n", i+1, d)
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

	analyses := make([]*Analysis, 0, len(dirs))
	for _, d := range dirs {
		a, err := analyze(d, st)
		if err != nil {
			fmt.Printf("目标: %s\n  [跳过] %v\n\n", d, err)
			continue
		}
		analyses = append(analyses, a)
	}

	if *status {
		for _, a := range analyses {
			printAnalysis(a)
		}
		printStateInfo(st)
		return
	}

	didWork := false
	for _, a := range analyses {
		printAnalysis(a)
		if a.Verdict == "already" && !*force && !*dry {
			fmt.Println("  -> 已是中文，跳过（要重打请加 -force）")
			fmt.Println()
			continue
		}
		if err := doPatch(a, *dry, *noBackup); err != nil {
			fmt.Printf("  [失败] %v\n\n", err)
			continue
		}
		didWork = true
		fmt.Println()
	}
	if !didWork && !*dry {
		fmt.Println("没有需要处理的安装。")
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
	sort.Strings(out)
	return out
}

func asarPath(d string) string { return filepath.Join(d, "resources", "ion-dist.asar") }
func exePath(d string) string  { return filepath.Join(d, "claude.exe") }

func statePath() string {
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "AnthropicClaude", "claude-hanhua.state.json")
}

func loadState() *State {
	b, err := os.ReadFile(statePath())
	if err != nil {
		return nil
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil
	}
	return &s
}

func saveState(s *State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(statePath(), b, 0644)
}

func versionOf(dir string) string {
	return strings.TrimPrefix(filepath.Base(dir), "app-")
}

// compareVersion 粗略比较 "4.23118.1" 这类版本号，返回 -1/0/1
func compareVersion(a, b string) int {
	pa := strings.Split(a, ".")
	pb := strings.Split(b, ".")
	n := len(pa)
	if len(pb) > n {
		n = len(pb)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
	}
	return 0
}

func warnNewerVersion(dirs []string, st *State) {
	if st == nil || st.Version == "" {
		return
	}
	newer := ""
	for _, d := range dirs {
		v := versionOf(d)
		if compareVersion(v, st.Version) > 0 {
			newer = v
		}
	}
	if newer != "" {
		fmt.Printf("[注意] 检测到更新版本 app-%s，上次汉化针对的是 app-%s。\n", newer, st.Version)
		fmt.Println("       旧版本资源不通用，需要对这个新版本重新汉化。")
		fmt.Println()
	}
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

// openAsar 读取 asar 结构并定位语言表条目
func openAsar(ap string) (*AsarInfo, error) {
	buf, err := os.ReadFile(ap)
	if err != nil {
		return nil, fmt.Errorf("读取 asar 失败（Claude 可能正在运行，先退出）: %w", err)
	}
	if len(buf) < 16 {
		return nil, errors.New("asar 文件损坏")
	}
	// asar 头部布局: [0:4]=4, [4:8]=headerSize(pickle 总长), [8:12]=headerSize-4,
	// [12:16]=header JSON 字节长度, 之后是 JSON 本体, 然后是 padding 到 8+headerSize
	headerSize := int64(binary.LittleEndian.Uint32(buf[4:8]))
	jsonLen := int64(binary.LittleEndian.Uint32(buf[12:16]))
	dataStart := 8 + headerSize
	headerEnd := 16 + jsonLen
	if headerEnd > int64(len(buf)) {
		return nil, errors.New("asar 头部长度异常")
	}
	headerRaw := string(buf[16:headerEnd])

	var root Entry
	if err := json.Unmarshal([]byte(headerRaw), &root); err != nil {
		return nil, fmt.Errorf("解析 asar 头部失败: %w", err)
	}
	ent := findEntry(&root, "", targetEntry)
	if ent == nil {
		return nil, fmt.Errorf("asar 里找不到 %s，可能这个版本结构不同", targetEntry)
	}
	if ent.Integrity == nil {
		return nil, errors.New("该条目没有完整性信息，格式不支持")
	}
	off, err := strconv.ParseInt(ent.Offset, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("offset 解析失败: %w", err)
	}
	abs := dataStart + off
	if abs+ent.Size > int64(len(buf)) {
		return nil, errors.New("条目位置越界")
	}
	sum := sha256.Sum256([]byte(headerRaw))
	return &AsarInfo{
		Buf:       buf,
		HeaderRaw: headerRaw,
		HeaderEnd: headerEnd,
		DataStart: dataStart,
		Entry:     ent,
		Abs:       abs,
		Content:   buf[abs : abs+ent.Size],
		HeaderSHA: hex.EncodeToString(sum[:]),
	}, nil
}

// countChinese 统计真正的「中文条目」：含汉字且不含日文假名。
// 只数汉字会把日文误判成中文（日文里大量用汉字，如「設定」「変更」），
// 所以必须靠假名（平假名/片假名）来排除日文。
func countChinese(m map[string]string) int {
	c := 0
	for _, v := range m {
		hasCJK, hasKana := false, false
		for _, r := range v {
			if r >= 0x4E00 && r <= 0x9FFF {
				hasCJK = true
			}
			if (r >= 0x3040 && r <= 0x309F) || (r >= 0x30A0 && r <= 0x30FF) || (r >= 0x31F0 && r <= 0x31FF) {
				hasKana = true
			}
		}
		if hasCJK && !hasKana {
			c++
		}
	}
	return c
}

func analyze(d string, st *State) (*Analysis, error) {
	info, err := openAsar(asarPath(d))
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimRight(info.Content, " \t\r\n\x00")
	var m map[string]string
	if err := json.Unmarshal(trimmed, &m); err != nil {
		return nil, fmt.Errorf("解析语言表失败: %w", err)
	}
	cn := countChinese(m)
	a := &Analysis{
		Dir:     d,
		Version: versionOf(d),
		Total:   len(m),
		Chinese: cn,
		Ratio:   float64(cn) / float64(len(m)+1),
		Info:    info,
		State:   st,
	}
	switch {
	case a.Ratio >= 0.90:
		a.Verdict = "already"
	case a.Ratio >= 0.30:
		// 日文原版里也有约 5% 条目不含假名（纯汉字短词），所以门槛不能设太低
		a.Verdict = "partial"
	case st != nil && st.Version == a.Version && st.HeaderHash != info.HeaderSHA:
		// 同一版本，文件指纹和记录不符，且内容不是中文 —— 文件被替换/覆盖过
		a.Verdict = "stale"
	case st != nil && st.Version != "" && compareVersion(a.Version, st.Version) > 0:
		// 比上次汉化的版本更新，且内容不是中文 —— 升级上来的新版本
		a.Verdict = "upgrade"
	default:
		a.Verdict = "never"
	}
	return a, nil
}

func printAnalysis(a *Analysis) {
	fmt.Printf("目标: %s\n", a.Dir)
	fmt.Printf("  语言表: %d 条，其中中文 %d 条（%.1f%%）\n", a.Total, a.Chinese, a.Ratio*100)
	switch a.Verdict {
	case "already":
		fmt.Println("  判断: 已经是中文")
		if a.State != nil && a.State.Version == a.Version && a.State.HeaderHash != a.Info.HeaderSHA {
			fmt.Println("        文件指纹与上次记录不同，但内容仍是中文（可能是别的工具又改过）")
		}
	case "partial":
		fmt.Println("  判断: 只有部分中文，可能上次汉化被部分覆盖")
	case "stale":
		fmt.Println("  判断: 汉化已失效 —— 文件被替换过，Claude 可能重新下载了同版本")
		if a.State != nil {
			fmt.Printf("        上次汉化: %s（工具 v%s，命中 %d 条）\n",
				formatTime(a.State.PatchedAt), a.State.ToolVersion, a.State.Hit)
		}
	case "upgrade":
		fmt.Println("  判断: 这是升级后的新版本，还没汉化")
		if a.State != nil {
			fmt.Printf("        上次汉化的是 app-%s（%s）\n", a.State.Version, formatTime(a.State.PatchedAt))
		}
	case "never":
		fmt.Println("  判断: 从未汉化过（原版日文界面）")
	}
}

func printStateInfo(st *State) {
	if st == nil {
		fmt.Println("\n没有找到汉化记录文件（说明还没跑过这个工具，或记录被删）")
		return
	}
	fmt.Printf("\n上次汉化记录:\n  时间: %s\n  工具版本: v%s\n  目录: %s\n  命中: %d 条\n",
		formatTime(st.PatchedAt), st.ToolVersion, st.InstallDir, st.Hit)
}

func formatTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format("2006-01-02 15:04:05")
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
	os.Remove(statePath())
	fmt.Printf("[完成] 已还原: %s\n", d)
	return nil
}

func doPatch(a *Analysis, dry, noBackup bool) error {
	d := a.Dir
	ap, ep := asarPath(d), exePath(d)
	info := a.Info

	zh, err := loadCatalog()
	if err != nil {
		return err
	}
	fmt.Printf("  内置词库: %d 条\n", len(zh))

	var ja map[string]string
	if err := json.Unmarshal(bytes.TrimRight(info.Content, " \t\r\n\x00"), &ja); err != nil {
		return fmt.Errorf("解析目标语言表失败: %w", err)
	}

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
	if int64(len(outBytes)) > info.Entry.Size {
		return fmt.Errorf("新语言表 %d 字节，超出槽位 %d 字节", len(outBytes), info.Entry.Size)
	}
	filled := make([]byte, info.Entry.Size)
	copy(filled, outBytes)
	for i := len(outBytes); i < len(filled); i++ {
		filled[i] = 0x20
	}

	if dry {
		fmt.Println("  [dry-run] 校验通过，未写入")
		return nil
	}

	if !noBackup {
		if _, err := os.Stat(ap + ".hanhua-bak"); err != nil {
			fmt.Println("  备份原文件...")
			if err := copyFile(ap, ap+".hanhua-bak"); err != nil {
				return fmt.Errorf("备份 asar 失败: %w", err)
			}
			if err := copyFile(ep, ep+".hanhua-bak"); err != nil {
				return fmt.Errorf("备份 exe 失败: %w", err)
			}
		}
	}

	buf := info.Buf
	oldHash := info.Entry.Integrity.Hash
	oldBlocks := append([]string{}, info.Entry.Integrity.Blocks...)
	oldHeaderHash := info.HeaderSHA

	copy(buf[info.Abs:info.Abs+info.Entry.Size], filled)
	if err := os.WriteFile(ap, buf, 0644); err != nil {
		return fmt.Errorf("写入 asar 失败: %w", err)
	}

	newSum := sha256.Sum256(filled)
	newHash := hex.EncodeToString(newSum[:])
	var newBlocks []string
	bs := info.Entry.Integrity.BlockSize
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

	if strings.Count(info.HeaderRaw, oldHash) != 1 {
		return errors.New("条目哈希在头部不唯一，放弃（请反馈这个情况）")
	}
	newHeader := strings.Replace(info.HeaderRaw, oldHash, newHash, 1)
	for i := range oldBlocks {
		if i >= len(newBlocks) {
			break
		}
		if strings.Count(newHeader, oldBlocks[i]) != 1 {
			return errors.New("分块哈希在头部不唯一，放弃")
		}
		newHeader = strings.Replace(newHeader, oldBlocks[i], newBlocks[i], 1)
	}
	if len(newHeader) != len(info.HeaderRaw) {
		return errors.New("头部长度变化，放弃")
	}
	copy(buf[16:info.HeaderEnd], []byte(newHeader))
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

	st, _ := os.Stat(ap)
	var mtime int64
	if st != nil {
		mtime = st.ModTime().Unix()
	}
	_ = saveState(&State{
		ToolVersion: toolVer,
		PatchedAt:   time.Now().Format(time.RFC3339),
		InstallDir:  d,
		Version:     versionOf(d),
		HeaderHash:  finalHash,
		AsarSize:    int64(len(buf)),
		AsarMTime:   mtime,
		Entries:     len(ja),
		Hit:         hit,
	})

	fmt.Println("  [完成] 汉化成功，重新打开 Claude 即可")
	fmt.Printf("  备份: %s.hanhua-bak\n", ap)
	return nil
}
