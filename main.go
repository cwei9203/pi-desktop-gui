package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/egoist/mygo"
)

func main() {
	name := "Pi GUI"
	if mygo.IsDev() {
		name += " Dev"
	}
	mygo.App.SetName(name)
	generating := os.Getenv("MYGO_GENERATE") != ""
	if !generating && !mygo.App.RequestSingleInstanceLock() {
		return
	}
	desktop := &Desktop{}
	var setupErr error
	if !generating {
		var dir string
		dir, setupErr = os.UserConfigDir()
		if setupErr == nil {
			desktop, setupErr = newDesktop(filepath.Join(dir, name))
		}
	}
	if desktop == nil {
		desktop = &Desktop{}
	}
	mygo.Bind(desktop)
	var window *mygo.Window
	openWindow := func() {
		if window != nil {
			window.Restore()
			window.Focus()
			return
		}
		window = mygo.NewWindow(mygo.WindowOptions{
			Title: "Pi GUI", URL: "/", Width: 1200, Height: 820,
			MinWidth: 760, MinHeight: 540, StateKey: "main",
			TitleBarStyle:   mygo.TitleBarHiddenInset,
			BackgroundColor: "light-dark(#f5f4f0, #242421)",
		})
		window.OnClosed(func() { window = nil })
		window.Page().OnWillNavigate(func(e *mygo.NavigateEvent) {
			e.PreventDefault()
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mygo.App.WhenReady(func() {
		if setupErr != nil {
			mygo.Dialog.Error("无法读取本地设置", setupErr.Error())
			mygo.App.Exit(1)
			return
		}
		go desktop.deliver(ctx)
		openWindow()
	})
	mygo.App.OnWindowAllClosed(func() {})
	mygo.App.OnActivate(func(visible bool) {
		if !visible {
			openWindow()
		}
	})
	mygo.App.OnSecondInstance(func(_ []string, _ string) { openWindow() })
	var quitting, cleaned atomic.Bool
	mygo.App.OnBeforeQuit(func(e *mygo.QuitEvent) {
		if cleaned.Load() {
			return
		}
		e.PreventDefault()
		if !quitting.CompareAndSwap(false, true) {
			return
		}
		go func() {
			defer quitting.Store(false)
			desktop.mu.Lock()
			desktop.quitting = true
			desktop.mu.Unlock()
			defer func() {
				if !cleaned.Load() {
					desktop.mu.Lock()
					desktop.quitting = false
					desktop.mu.Unlock()
				}
			}()
			desktop.op.Lock()
			defer desktop.op.Unlock()
			if desktop.busy() {
				answer, err := mygo.Dialog.Message(mygo.MessageOptions{
					Type: mygo.MessageQuestion, Message: "停止当前任务并退出？",
					Detail: "已执行的文件修改会保留。", Buttons: []string{"停止并退出", "取消"},
					DefaultButton: 1, CancelButton: 1,
				})
				if err != nil || answer.Button != 0 {
					return
				}
				_, _ = desktop.Stop()
			}
			err := desktop.disconnect()
			if err != nil {
				mygo.Dialog.Error("无法结束 Pi 进程", err.Error())
				return
			}
			cleaned.Store(true)
			mygo.App.Quit()
		}()
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}
