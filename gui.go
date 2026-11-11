package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
)

type guiApp struct {
	mw         *walk.MainWindow
	logBox     *walk.TextEdit
	statusLbl  *walk.Label
	btnPatch   *walk.PushButton
	btnForce   *walk.PushButton
	btnRestore *walk.PushButton
	btnRefresh *walk.PushButton
	logBuf     strings.Builder
	busy       bool
}

func runGUI() {
	g := &guiApp{}

	if err := (MainWindow{
		AssignTo: &g.mw,
		Title:    fmt.Sprintf("%s v%s", toolName, toolVer),
		MinSize:  Size{Width: 720, Height: 520},
		Size:     Size{Width: 880, Height: 660},
		Layout:   VBox{Margins: Margins{Left: 12, Top: 12, Right: 12, Bottom: 12}, Spacing: 10},
		Children: []Widget{
			Label{
				AssignTo: &g.statusLbl,
				Text:     "正在检测 Claude 安装...",
			},
			Composite{
				Layout: HBox{Spacing: 8},
				Children: []Widget{
					PushButton{
						AssignTo:  &g.btnPatch,
						Text:      "一键汉化",
						OnClicked: func() { g.start(false, false) },
					},
					PushButton{
						AssignTo:  &g.btnForce,
						Text:      "强制重打",
						OnClicked: func() { g.start(false, true) },
					},
					PushButton{
						AssignTo:  &g.btnRestore,
						Text:      "还原原版",
						OnClicked: func() { g.start(true, false) },
					},
					PushButton{
						AssignTo:  &g.btnRefresh,
						Text:      "刷新状态",
						OnClicked: func() { g.refresh() },
					},
				},
			},
			TextEdit{
				AssignTo: &g.logBox,
				ReadOnly: true,
				VScroll:  true,
				HScroll:  true,
			},
		},
	}).Create(); err != nil {
		walk.MsgBox(nil, "启动失败", err.Error(), walk.MsgBoxIconError)
		return
	}

	g.refresh()
	g.mw.Run()
}

// setLog 写日志框。这个 TextEdit 不能设 ReadOnly：
// walk 内部对只读控件写文本会被系统忽略，日志框会一直是空的。
// setLog 写日志框
func (g *guiApp) setLog(s string) {
	_ = g.logBox.SetText(s)
}

// emit 供后台流程实时追加日志
func (g *guiApp) emit(s string) {
	g.logBuf.WriteString(s)
	content := g.logBuf.String()
	g.mw.Synchronize(func() { g.setLog(content) })
}

func (g *guiApp) setEnabled(on bool) {
	g.btnPatch.SetEnabled(on)
	g.btnForce.SetEnabled(on)
	g.btnRestore.SetEnabled(on)
	g.btnRefresh.SetEnabled(on)
}

func (g *guiApp) start(restore, force bool) {
	if g.busy {
		return
	}
	g.busy = true
	g.setEnabled(false)
	g.logBuf.Reset()
	g.setLog("")
	g.statusLbl.SetText("正在执行，请稍候...")

	go func() {
		logEmit = g.emit
		defer func() { logEmit = nil }()

		begin := time.Now()
		err := runOnce(Options{Restore: restore, Force: force})
		cost := time.Since(begin)

		g.mw.Synchronize(func() {
			g.busy = false
			g.setEnabled(true)
			if err != nil {
				g.statusLbl.SetText("失败：" + err.Error())
				return
			}
			if restore {
				g.statusLbl.SetText(fmt.Sprintf("还原完成（%.1f 秒）", cost.Seconds()))
			} else {
				g.statusLbl.SetText(fmt.Sprintf("完成（%.1f 秒）—— 现在可以打开 Claude 了", cost.Seconds()))
			}
		})

		if err != nil {
			msg := err.Error()
			g.mw.Synchronize(func() {
				walk.MsgBox(g.mw, "执行失败", msg, walk.MsgBoxIconError)
			})
			return
		}
		title, body := "完成", "汉化完成，重新打开 Claude 即可。"
		if restore {
			title, body = "完成", "已还原成原版，重新打开 Claude 即可。"
		}
		g.mw.Synchronize(func() {
			walk.MsgBox(g.mw, title, body, walk.MsgBoxIconInformation)
		})
	}()
}

func (g *guiApp) refresh() {
	if g.busy {
		return
	}
	g.statusLbl.SetText("正在检测...")
	go func() {
		dirs := detectDirs()
		st := loadState()

		var sb strings.Builder
		if len(dirs) == 0 {
			sb.WriteString("没有找到 Claude 桌面版安装目录。\n\n")
			sb.WriteString("查找位置: %LOCALAPPDATA%\\AnthropicClaude\\app-<版本>\\\n")
			sb.WriteString("如果 Claude 装在别处，请先用命令行 -dir 指定。\n")
			g.mw.Synchronize(func() {
				g.statusLbl.SetText("没有检测到 Claude 安装")
				g.logBuf.Reset()
				g.logBuf.WriteString(sb.String())
				g.setLog(sb.String())
			})
			return
		}

		need := 0
		names := make([]string, 0, len(dirs))
		for _, d := range dirs {
			a, err := analyze(d, st)
			if err != nil {
				fmt.Fprintf(&sb, "目标: %s\n  [无法读取] %v\n\n", d, err)
				names = append(names, "app-"+versionOf(d)+"（读取失败）")
				continue
			}
			sb.WriteString(renderAnalysis(a))
			sb.WriteString("\n")
			if a.Verdict == "already" {
				names = append(names, "app-"+a.Version+"（已中文）")
			} else {
				names = append(names, "app-"+a.Version+"（需要汉化）")
				need++
			}
		}
		sb.WriteString(renderState(st))

		summary := "检测到 " + strings.Join(names, "、")
		if need > 0 {
			summary += fmt.Sprintf("　→ 有 %d 个需要汉化，点「一键汉化」", need)
		} else {
			summary += "　→ 都已汉化，无需处理"
		}
		content := sb.String()

		g.mw.Synchronize(func() {
			g.statusLbl.SetText(summary)
			g.logBuf.Reset()
			g.logBuf.WriteString(content)
			g.setLog(content)
		})
	}()
}

// renderAnalysis 与 printAnalysis 内容一致，只是返回字符串
func renderAnalysis(a *Analysis) string {
	var b strings.Builder
	fmt.Fprintf(&b, "目标: %s\n", a.Dir)
	fmt.Fprintf(&b, "  语言表: %d 条，其中中文 %d 条（%.1f%%）\n", a.Total, a.Chinese, a.Ratio*100)
	switch a.Verdict {
	case "already":
		b.WriteString("  判断: 已经是中文\n")
	case "partial":
		b.WriteString("  判断: 只有部分中文，可能上次汉化被部分覆盖\n")
	case "stale":
		b.WriteString("  判断: 汉化已失效 —— 文件被替换过，Claude 可能重新下载了同版本\n")
		if a.State != nil {
			fmt.Fprintf(&b, "        上次汉化: %s（工具 v%s，命中 %d 条）\n",
				formatTime(a.State.PatchedAt), a.State.ToolVersion, a.State.Hit)
		}
	case "upgrade":
		b.WriteString("  判断: 这是升级后的新版本，还没汉化\n")
		if a.State != nil {
			fmt.Fprintf(&b, "        上次汉化的是 app-%s（%s）\n", a.State.Version, formatTime(a.State.PatchedAt))
		}
	case "never":
		b.WriteString("  判断: 从未汉化过（原版日文界面）\n")
	}
	return b.String()
}

func renderState(st *State) string {
	if st == nil {
		return "上次汉化记录: 无（还没跑过这个工具）\n"
	}
	return fmt.Sprintf("上次汉化记录: %s　工具 v%s　目录 app-%s　命中 %d 条\n",
		formatTime(st.PatchedAt), st.ToolVersion, st.Version, st.Hit)
}
