package scaffold

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

//go:embed templates/go/* templates/typescript/* templates/python/*
var templatesFS embed.FS

type Options struct {
	Lang     string
	Name     string
	Endpoint string
	Force    bool
}

type templateData struct {
	Name     string
	Endpoint string
}

var supported = map[string]string{
	"go":         "templates/go",
	"typescript": "templates/typescript",
	"ts":         "templates/typescript",
	"python":     "templates/python",
	"py":         "templates/python",
}

func Init(opt Options) error {
	root, ok := supported[strings.ToLower(opt.Lang)]
	if !ok {
		return fmt.Errorf("unsupported language %q (supported: go, typescript, python)", opt.Lang)
	}

	target, err := filepath.Abs(opt.Name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(target); err == nil {
		if !opt.Force {
			return fmt.Errorf("directory %s already exists (use --force to overwrite)", target)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	data := templateData{Name: opt.Name, Endpoint: opt.Endpoint}

	return fs.WalkDir(templatesFS, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(target, 0o755)
		}
		// Strip a trailing .tmpl suffix from rendered file paths so the template
		// system can ship Go-template-rendered files without breaking host tooling
		// (e.g. a TS toolchain does not have to ignore raw .go.tmpl files).
		outRel := strings.TrimSuffix(rel, ".tmpl")
		outPath := filepath.Join(target, outRel)

		if d.IsDir() {
			return os.MkdirAll(outPath, 0o755)
		}

		raw, err := fs.ReadFile(templatesFS, path)
		if err != nil {
			return err
		}

		var rendered []byte
		if strings.HasSuffix(rel, ".tmpl") {
			tpl, err := template.New(rel).Parse(string(raw))
			if err != nil {
				return fmt.Errorf("template %s: %w", rel, err)
			}
			var sb strings.Builder
			if err := tpl.Execute(&sb, data); err != nil {
				return fmt.Errorf("render %s: %w", rel, err)
			}
			rendered = []byte(sb.String())
		} else {
			rendered = raw
		}

		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return err
		}
		return os.WriteFile(outPath, rendered, 0o644)
	})
}
