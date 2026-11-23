# Claude 桌面版一键汉化（Windows）

把 Windows 版 Claude Desktop 的日文/英文界面换成简体中文，一个 exe，双击就行。

## 这是什么

官方 Claude Desktop 的语言包只有 11 种，**没有中文**。你如果在日本区或者账号 locale 被设成 `ja-JP`，整个界面就是日文。

这个工具把内置的 **50,140 条**中文词库注入到 Claude 的语言资源包里，并同步修正 Electron 的完整性校验，让应用正常启动、界面显示中文。

工具启动时会先自检，再决定要不要动手：

- **已汉化且文件没变** → 提示「已是中文」，直接跳过
- **Claude 升级过、资源被换掉** → 提示「检测到新版本」，自动重新汉化
- **有多个版本目录** → 逐个判断，只处理需要处理的

**当前效果：50,140 条词库，Claude 6.28899.6 实测四张语言表全部覆盖，零日文残留**

| 语言表 | 位置 | 条数 | 覆盖 |
|---|---|---|---|
| 主界面文案 | `ion-dist.asar` → `i18n/ja-JP.json` | 46,448 | 100% |
| 动态文案 | `ion-dist.asar` → `i18n/dynamic/ja-JP.json` | 51 | 100% |
| 主进程 / 菜单 | `app.asar` → `resources/i18n/ja-JP.json` | 1,575 | 100% |
| 窗口壳层 | `app.asar` → `resources/frame/shell/i18n/frame-shell/ja-JP.json` | 1,901 | 100% |

| 原日文 | 汉化后 |
|---|---|
| 新規タスク - Claude | 新会话 - Claude |
| サイドバーを非表示 | 隐藏侧边栏 |
| 新規作成 / 検索 / カスタマイズ | 新建 / 搜索 / 自定义 |
| ようこそ！ | 欢迎！ |
| Claude へのプロンプトを入力してください | 请输入给 Claude 的提示词 |
| ファイルやコネクタなどを追加 | 添加文件、连接器等 |
| タスクを開始 | 开始任务 |
| プロバイダーに認証情報が拒否されました | 提供方拒绝了凭据。请在「设置」中重新输入。 |

剩下的非中文条目都是英文产品名、快捷键和纯占位符（`Cowork`、`Haiku`、`{name} · {state}`），刻意不译。

## 用法

**双击打开，窗口里点按钮就行。** 不用记命令行，也不会闪一下就没。

窗口里有四个按钮：

| 按钮 | 作用 |
|---|---|
| 一键汉化 | 已汉化就跳过，Claude 升级过就自动重打 |
| 强制重打 | 无视「已是中文」的判断，把词库重新注入一遍 |
| 还原原版 | 从备份恢复成原版日文界面 |
| 刷新状态 | 重新检测当前状态 |

窗口顶部状态行一句话说清现状（已中文 / 需要汉化）。下面日志框写每个安装目录的细节：语言表多少条、中文占多少、上次汉化是什么时候。跑完弹提示框，点掉就能去开 Claude 了。

命令行参数仍然保留，加上任一参数就不弹窗口（自动化、脚本里用）：

```
claude-hanhua.exe -status      只看状态诊断（是否汉化过、是否升级了）
claude-hanhua.exe -list        列出检测到的 Claude 安装
claude-hanhua.exe -dry         只检查不写入（看命中率）
claude-hanhua.exe -dir <路径>  指定安装目录
claude-hanhua.exe -restore     还原成原版
claude-hanhua.exe -force       强制重新汉化（忽略「已是中文」判断）
claude-hanhua.exe -no-backup   不备份（不建议）
```

**升级自检是怎么做的**：每次汉化成功会写一份记录到

```
%LOCALAPPDATA%\AnthropicClaude\claude-hanhua.state.json
```

里面存着当时的版本号、资源包头部哈希、命中条数。下次运行就是拿这份记录和磁盘现状对比——哈希对不上说明文件被换过，版本号变大了说明换了新版本。同时也会统计当前语言表里「含汉字且不含日文假名」的条目占比，判断到底有没有中文。两路信息交叉验证，避免误判。

安装目录默认在：

```
%LOCALAPPDATA%\AnthropicClaude\app-<版本>\
```

## 它到底改了什么

Claude 的资源包是 Electron 的 `asar` 格式，改里面的文件会踩到三层校验，少改一层应用直接闪退：

1. **条目级 SHA256**：asar 头部 JSON 里每个文件有自己的 `integrity.hash`
2. **分块哈希**：每个条目还有 `blocks` 数组，按 `blockSize` 切块算 SHA256
3. **头部哈希**：整个头部 JSON 的 SHA256 被嵌在 `claude.exe` 的 PE 资源里

Claude 6 起界面文案分散在两张 asar、四个语言表里，所以要处理两次：

1. 读 `resources\ion-dist.asar`，定位 `i18n/ja-JP.json`（4.97MB）和 `i18n/dynamic/ja-JP.json`
2. 读 `resources\app.asar`，定位 `resources/i18n/ja-JP.json` 和窗口壳层那份
3. 用内置中文词库按 key 合并：命中用中文，没命中保留原文
4. 写回原槽位，不足部分空格填充（保持长度不变）
5. 重算条目 SHA256 + 每个分块的 SHA256，替换头部 JSON 里的旧值
6. 重算头部 SHA256，替换 `claude.exe` 里记录的旧值
7. 备份原文件为 `*.hanhua-bak`，出问题可以 `-restore`

两个细节踩过坑，写在这里省得别人再踩：

- **小条目的整文件哈希和第一个分块哈希是同一个字符串**（一个分块就装完了）。先替换分块再替换整文件，且必须用「全部替换」而不是「替换一次」，否则尾部会剩一半旧值。
- **重复运行要能对上 exe 里的旧哈希**。exe 里可能是原版哈希、上一次汉化的哈希，或者上次写 exe 失败留下的中间态。工具把「当前值 + 备份原版值 + 上次记录值」都当候选，逐个在 exe 里试，命中哪个换哪个。

## 备份与还原

首次汉化会自动备份：

```
resources\ion-dist.asar.hanhua-bak   原版资源包
resources\app.asar.hanhua-bak        原版资源包
claude.exe.hanhua-bak                原版 exe
```

还原：

```
claude-hanhua.exe -restore
```

## 注意事项

**Claude 一升级，汉化就失效——但工具能自己发现。** 官方更新会把资源包和 `claude.exe` 整个换掉，并装到一个新的 `app-<版本>` 目录里。下次运行工具时会自动识别「新版本未汉化」并处理，旧版本目录里的汉化不受影响。如果新版本改了 key，命中率会下降但不会崩——没命中的条目保留原文，跑 `-dry` 就能看到命中多少。

**汉化前先退出 Claude。** 资源包和 exe 被占用时写不进去，工具会提示失败；退出应用再点一次即可。

**只对 Squirrel 安装版有效。** 也就是 `%LOCALAPPDATA%\AnthropicClaude\app-*` 这种目录结构。Microsoft Store 的 MSIX 版装在 `C:\Program Files\WindowsApps`，受系统保护，工具默认不改它。

**界面语言取决于账号 locale。** 工具改的是 `ja-JP` 语言表。如果你的 Claude 界面不是日文（比如英文），那它读的是 `en-US.json`，这个工具就不适用。判断方法：跑 `-dry`，如果显示「目标语言表: N 条」且命中率很高，就说明对上了。

**汉化不影响功能，但也不改变区域限制。** 如果你的账号碰到「Claude isn't available in your region」，那是账号/网络层面的问题，跟界面语言无关。

## 词库来源

- 约 6,959 条来自社区项目 [ooac/Claude-desk-cn](https://github.com/ooac/Claude-desk-cn)（macOS 版汉化补丁）
- 其余为针对 Windows 版 key 体系的批量翻译：v1.0–v1.2 覆盖 Claude 5.1231.2，v1.3.0 补齐 Claude 6.28899.6 新增的 5,941 条（含主进程菜单与壳层两份表）

翻译校验规则（每批都跑）：

- 键集合与原文完全一致
- 每个值里的 `{变量名}` 集合与原文一致
- ICU 的 plural / select / number 结构不动（`{count, plural, one {…} other {…}}` 里的自然语言照译，语法关键字不译）
- HTML 标签（`<b>`、`<link>`、`<code>`）不丢
- 译文里不许有日文假名残留
- 产品名保持英文（Claude、Anthropic、Artifacts→作品、Cowork、Slack、AWS、MCP…）

## 自己编译

需要 Go 1.21+。界面用的是 [lxn/walk](https://github.com/lxn/walk)（Windows 原生控件，纯 Go，不需要 CGO）。

```powershell
# 1. 拉依赖
go get github.com/lxn/walk
go install github.com/akavel/rsrc@latest

# 2. 生成 Windows 资源（清单文件，GUI 控件必须要有，否则样式退回老古董）
rsrc -manifest app.manifest -o rsrc.syso -arch amd64

# 3. 编译。-H=windowsgui 让程序不带控制台窗口
$env:CGO_ENABLED='0'
go build -ldflags="-s -w -H=windowsgui" -trimpath -o claude-hanhua.exe .
```

词库 `zh.json.gz` 通过 `go:embed` 编进 exe，所以产物是单文件、无运行时依赖的。

## 免责声明

这是非官方的界面语言修改工具，修改的是本地安装文件，不涉及账号、不联网、不上传任何数据。使用前建议先备份，风险自负。Claude 是 Anthropic 的产品，本项目与 Anthropic 无关。

## License

MIT
