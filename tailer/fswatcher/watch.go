// Copyright 2019 The grok_exporter Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package fswatcher

import (
	"io"
	"io/ioutil"
	"path/filepath"
	"strings"

	"github.com/davecgh/go-spew/spew"
	"github.com/fsnotify/fsnotify"
	"github.com/hpcloud/tail"
	"github.com/sirupsen/logrus"

	"github.com/fstab/grok_exporter/tailer/glob"
)

type watcher struct {
	globs        []glob.Glob
	tailConfig   tail.Config
	logger       logrus.FieldLogger
	watcher      *fsnotify.Watcher
	watchedDirs  map[string]struct{}
	watchedFiles map[string]*tailer
	lines        chan *Line
	errors       chan Error
	done         chan struct{}
}

// TODO: failOnMissingFile 的实现
func RunFileTailer(globs []glob.Glob, readall bool, failOnMissingFile bool, log logrus.FieldLogger) (Interface, error) {
	dirs, Err := expandGlobs(globs)
	if Err != nil {
		return nil, Err
	}

	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	tailConfig := tail.Config{
		Location: &tail.SeekInfo{
			Offset: 0,
			Whence: io.SeekEnd,
		},
		ReOpen:    true,
		MustExist: failOnMissingFile,
		Follow:    true,
	}
	if readall {
		tailConfig.Location.Whence = io.SeekStart
	}

	w := &watcher{
		globs:        globs,
		tailConfig:   tailConfig,
		logger:       log.WithField("component", "watcher"),
		watcher:      fw,
		watchedFiles: make(map[string]*tailer),
		lines:        make(chan *Line),
		errors:       make(chan Error),
		done:         make(chan struct{}),
	}
	w.init(dirs)
	go w.run()
	return w, nil
}

func (w *watcher) run() {
	for {
		select {
		case event, ok := <-w.watcher.Events:
			if !ok {
				continue
			}
			w.logger.Debug(spew.Sprintf("recv event %#v", event))
			w.handle(event)
		case <-w.done:
			for _, t := range w.watchedFiles {
				t.stop()
			}
			return
		}
	}
}

// list watchedDirs，获取所有需要监听的文件
func (w *watcher) init(dirs map[string]struct{}) {
	for dir := range dirs {
		fis, err := ioutil.ReadDir(dir)
		if err != nil {
			w.errors <- NewErrorf(NotSpecified, err, "read dir %s failed", dir)
			continue
		}
		if err := w.watcher.Add(dir); err != nil {
			w.errors <- NewErrorf(NotSpecified, err, "watch dir %s failed", dir)
		}
		for _, fi := range fis {
			path := filepath.Join(dir, fi.Name())
			w.watch(path)
		}
	}
}

// TODO: 重命名文件会令grok从头重读该文件，多数系统不支持MovedFromTo事件
// TODO: case "CHMOD": 权限不足也应算一种删除
func (w *watcher) handle(event fsnotify.Event) {
	path := event.Name
	ops := strings.Split(event.Op.String(), "|")
	for _, op := range ops {
		switch op {
		case "CREATE":
			if matchGlobs(path, w.globs) {
				w.watch(path)
			}
		case "RENAME":
			fallthrough
		case "REMOVE":
			w.unwatch(path)
		}
	}
}

func (w *watcher) watch(path string) {
	if _, existing := w.watchedFiles[path]; existing {
		return
	}
	t, err := w.newTailer(path)
	if err != nil {
		w.errors <- NewErrorf(NotSpecified, err, "watch file %s failed", path)
		return
	}
	w.watchedFiles[path] = t
	go t.run()
}

func (w *watcher) unwatch(path string) {
	t, existing := w.watchedFiles[path]
	if !existing {
		return
	}
	t.stop()
	delete(w.watchedFiles, path)
}

func (w *watcher) Lines() chan *Line {
	return w.lines
}

func (w *watcher) Errors() chan Error {
	return w.errors
}

func (w *watcher) Close() {
	close(w.done)
}
