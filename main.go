package main

import (
	"archive/zip"
	"bufio"
	"encoding/csv"
	"flag"
	"fmt"
	"html"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	reStyleBlock = regexp.MustCompile(`(?is)<style.*?>.*?</style>`)
	reTransDiv   = regexp.MustCompile(`(?is)<div\s+class="dc-line\s+dc-translation[^"]*"\s*>(.*?)</div>`)
	reCloze      = regexp.MustCompile(`(?is)\{\{c\d+::(.*?)(?:::[^}]*)?\}\}`)
	reImage      = regexp.MustCompile(`(?is)<div\s+class="([^"]*\bdc-image[^"]*)"\s+style="([^"]*?)"\s*></div>`)
	reBgImage    = regexp.MustCompile(`(?is)background-image\s*:\s*url\(([^)]+)\)`)
	reCardDiv    = regexp.MustCompile(`(?is)<div\s+class="([^"]*\bdc-card[^"]*)"(.*?)>`)
	reUnwrapDown = regexp.MustCompile(`(?is)<span[^>]*class="[^"]*\bdc-down\b[^"]*"[^>]*>(.*?)</span>`)
	reUnwrapGap  = regexp.MustCompile(`(?is)<span[^>]*class="[^"]*\bdc-gap\b[^"]*"[^>]*>(.*?)</span>`)
)

func main() {
    // 使い方:
    //  1) すべてのZIP: go run main.go
    //  2) 特定ZIPのみ: go run main.go -in data/raw/lln_anki_items_2025-8-9_update_542740.zip
    //  3) 単体TSV    : go run main.go -in items.csv
    in := flag.String("in", "", "input ZIP or TSV file (optional). When empty, process all ZIPs under -rawdir")
    rawdir := flag.String("rawdir", "data/raw", "directory containing zip files")
    procdir := flag.String("procdir", "data/processed", "directory to write outputs")
    color := flag.String("color", "rgb(255, 189, 128)", "color for cloze terms (e.g. #e91e63 or red)")
    flag.Parse()

    check(os.MkdirAll(*procdir, 0o755))

    switch {
    case *in != "":
        // -in に .zip または .tsv/.csv を指定可能
        ext := strings.ToLower(filepath.Ext(*in))
        out := filepath.Join(*procdir, baseNameNoExt(*in)+"_out.tsv")
        if ext == ".zip" {
            check(processZip(*in, out, *color))
            fmt.Printf("OK(zip): %s -> %s\n", *in, out)
        } else {
            check(processTSVFile(*in, out, *color))
            fmt.Printf("OK(tsv): %s -> %s\n", *in, out)
        }

    default:
        // rawdir 内のZIP一括処理
        entries, err := os.ReadDir(*rawdir)
        check(err)

        foundZip := false
        for _, e := range entries {
            if e.IsDir() {
                continue
            }
            if strings.HasSuffix(strings.ToLower(e.Name()), ".zip") {
                foundZip = true
                zipPath := filepath.Join(*rawdir, e.Name())
                out := filepath.Join(*procdir, baseNameNoExt(zipPath)+"_out.tsv")
                check(processZip(zipPath, out, *color))
                fmt.Printf("OK: %s -> %s\n", zipPath, out)
            }
        }
        if !foundZip {
            fmt.Printf("no zip files found in %s\n", *rawdir)
        }
    }
}

// ----- ZIP 内の item.csv を探して処理 -----
func processZip(zipPath, outPath, color string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()

	var target *zip.File
	var fallback *zip.File

	for _, f := range zr.File {
		base := strings.ToLower(path.Base(f.Name)) // zip 内は常に '/' 区切り
		switch base {
		case "item.csv", "items.csv":
			target = f
			break
		default:
			if strings.HasSuffix(base, ".csv") {
				// 念のためCSVが1個だけのケースに備えて候補も保持
				fallback = f
			}
		}
	}
	if target == nil {
		target = fallback
	}
	if target == nil {
		return fmt.Errorf("item.csv not found in %s", zipPath)
	}

	rc, err := target.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	return processTSV(rc, outPath, color)
}

// ----- 単体TSVファイルを処理 -----
func processTSVFile(inPath, outPath, color string) error {
	inFile, err := os.Open(inPath)
	if err != nil {
		return err
	}
	defer inFile.Close()
	return processTSV(inFile, outPath, color)
}

// ----- TSV ストリームを処理 -----
func processTSV(r io.Reader, outPath, color string) error {
	br := bufio.NewReader(r)
	// UTF-8 BOM 対策（あれば除去）
	br = stripBOM(br)

	check(os.MkdirAll(filepath.Dir(outPath), 0o755))
	outFile, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer outFile.Close()

	cr := csv.NewReader(br)
	cr.Comma = '\t'
	cr.LazyQuotes = true

	cw := csv.NewWriter(outFile)
	cw.Comma = '\t'

	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if len(rec) == 0 {
			continue
		}

		htmlIn := rec[0]
		sound := ""
		if len(rec) >= 2 {
			sound = strings.TrimSpace(rec[1])
		}

		htmlOut, ja := transform(htmlIn, sound, color)
		if err := cw.Write([]string{htmlOut, ja}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func transform(h, sound, color string) (string, string) {
	// 1) <style>…</style> を削除
	h = reStyleBlock.ReplaceAllString(h, "")

	// 2) 訳文抽出 → 本文から訳divを削除
	ja := ""
	if m := reTransDiv.FindStringSubmatch(h); len(m) >= 2 {
		ja = strings.TrimSpace(stripTags(m[1]))
		h = reTransDiv.ReplaceAllString(h, "")
	}

	// 3) Cloze → タグを除去したプレーンテキストにして、色付きで差し替え
	h = reCloze.ReplaceAllStringFunc(h, func(s string) string {
		g := reCloze.FindStringSubmatch(s)
		raw := ""
		if len(g) >= 2 {
			raw = g[1]
		}
		plain := strings.TrimSpace(stripTags(raw))
		if plain == "" {
			return ""
		}
		return `<span style="color:` + color + `;">` + html.EscapeString(plain) + `</span>`
	})

	// 3.5) Cloze外に残る dc-down / dc-gap の <span> を剥がす
	for {
		before := h
		h = reUnwrapDown.ReplaceAllString(h, "$1")
		h = reUnwrapGap.ReplaceAllString(h, "$1")
		if h == before {
			break
		}
	}

	// 4) .dc-card → 下線なしの inline スタイル
	h = reCardDiv.ReplaceAllString(h, `<div class="$1" style="padding-bottom:1rem;">`)

	// 5) 画像ボックスにサイズ等を inline 付与
	h = reImage.ReplaceAllStringFunc(h, func(s string) string {
		p := reImage.FindStringSubmatch(s)
		if len(p) < 3 {
			return s
		}
		class := p[1]
		style := p[2]

		bg := ""
		if m := reBgImage.FindStringSubmatch(style); len(m) >= 2 {
			bg = m[0]
			if !strings.HasSuffix(bg, ";") {
				bg += ";"
			}
		}
		newStyle := strings.Join([]string{
			"display:inline-block",
			"width:calc(50% - 10px)",
			"padding-bottom:29%",
			"background-position:center",
			"background-repeat:no-repeat",
			"background-size:cover",
			"margin-left:2px",
			"margin-right:2px",
		}, ";") + ";"
		newStyle += bg
		return `<div class="` + class + `" style="` + newStyle + `"></div>`
	})

	// 6) 英文直下に音声を差し込み
	if s := strings.Trim(sound, `" `); s != "" {
		if !strings.HasPrefix(s, "[sound:") {
			if i := strings.Index(s, "[sound:"); i >= 0 {
				s = s[i:]
			}
		}
		if idx := strings.Index(h, `<div class="dc-line"`); idx >= 0 {
			if end := strings.Index(h[idx:], `</div>`); end >= 0 {
				insert := idx + end + len(`</div>`)
				audio := `<div class="dc-audio" style="padding:0.4rem;margin-top:0.25rem;">` + s + `</div>`
				h = h[:insert] + audio + h[insert:]
			}
		}
	}

	// 軽い整形
	h = strings.ReplaceAll(h, "  ", " ")
	return h, ja
}

// 訳文抽出や cloze 内のタグ除去に使用
func stripTags(s string) string {
	inTag := false
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '<':
			inTag = true
		case '>':
			inTag = false
		default:
			if !inTag {
				b.WriteRune(r)
			}
		}
	}
	return strings.TrimSpace(html.UnescapeString(b.String()))
}

func stripBOM(r *bufio.Reader) *bufio.Reader {
	if b, _ := r.Peek(3); len(b) == 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		_, _ = r.Discard(3)
	}
	return r
}

func baseNameNoExt(p string) string {
	base := filepath.Base(p)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}
