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
    // div直書きの翻訳行（後方互換）
    reTransDiv   = regexp.MustCompile(`(?is)<div\s+class="dc-line\s+dc-translation[^"]*"\s*>(.*?)</div>`)

    // 場所やタグを問わず、最初に見つかった .dc-translation を拾えるように
    reAnyTranslation = regexp.MustCompile(`(?is)<[^>]+class="[^"]*\bdc-translation\b[^"]*"[^>]*>(.*?)</[^>]+>`)

    // Cloze の後半もキャプチャする
    reCloze      = regexp.MustCompile(`(?is)\{\{c\d+::(.*?)(?:::(.*?))?\}\}`)

    reImage      = regexp.MustCompile(`(?is)<div\s+class="([^"]*\bdc-image[^"]*)"\s+style="([^"]*?)"\s*></div>`)
    reBgImage    = regexp.MustCompile(`(?is)background-image\s*:\s*url\(([^)]+)\)`)
    reCardDiv    = regexp.MustCompile(`(?is)<div\s+class="([^"]*\bdc-card[^"]*)"(.*?)>`)
    reUnwrapDown = regexp.MustCompile(`(?is)<span[^>]*class="[^"]*\bdc-down\b[^"]*"[^>]*>(.*?)</span>`)
    reUnwrapGap  = regexp.MustCompile(`(?is)<span[^>]*class="[^"]*\bdc-gap\b[^"]*"[^>]*>(.*?)</span>`)
)


func main() {
	// 使い方:
	//  1) すべてのZIPを差分処理: go run .
	//  2) 特定ZIPのみ処理     : go run . -in data/raw/xxx.zip
	//  3) 単体TSVを処理       : go run . -in items.csv
	in := flag.String("in", "", "input ZIP or TSV file (optional). When empty, process all ZIPs under -rawdir")
	rawdir := flag.String("rawdir", "data/raw", "directory containing zip files")
	procdir := flag.String("procdir", "data/processed", "directory to write outputs")
	color := flag.String("color", "rgb(255, 189, 128)", "color for cloze terms (e.g. #e91e63 or red)")

	ankiMedia := flag.String("ankimedia",
		"/Users/nakaokashinzo/Library/Application Support/Anki2/ユーザー 1/collection.media",
		"path to Anki collection.media")
	overwrite := flag.Bool("overwrite", true, "overwrite media files in Anki media folder")
	force := flag.Bool("force", false, "reprocess even if output TSV already exists")

	flag.Parse()

	check(os.MkdirAll(*procdir, 0o755))
	check(os.MkdirAll(*ankiMedia, 0o755))

	switch {
	case *in != "":
		ext := strings.ToLower(filepath.Ext(*in))
		out := filepath.Join(*procdir, baseNameNoExt(*in)+"_out.tsv")
		if ext == ".zip" {
			check(processZip(*in, out, *color, *ankiMedia, *overwrite))
			fmt.Printf("OK(zip): %s -> %s\n", *in, out)
		} else {
			check(processTSVFile(*in, out, *color))
			fmt.Printf("OK(tsv): %s -> %s\n", *in, out)
		}
	default:
		entries, err := os.ReadDir(*rawdir)
		check(err)

		foundZip := false
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if !strings.HasSuffix(strings.ToLower(e.Name()), ".zip") {
				continue
			}
			foundZip = true
			zipPath := filepath.Join(*rawdir, e.Name())
			out := filepath.Join(*procdir, baseNameNoExt(zipPath)+"_out.tsv")

			if !*force && fileExists(out) {
				fmt.Printf("skip (exists): %s -> %s\n", zipPath, out)
				continue
			}

			check(processZip(zipPath, out, *color, *ankiMedia, *overwrite))
			fmt.Printf("OK: %s -> %s\n", zipPath, out)
		}
		if !foundZip {
			fmt.Printf("no zip files found in %s\n", *rawdir)
		}
	}
}

// ----- ZIP を処理：item.csv を変換 + media/ を Anki にコピー -----
func processZip(zipPath, outPath, color, ankiMedia string, overwrite bool) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()

	var target *zip.File
	var fallback *zip.File

	// media コピー
	copied, skipped, err := copyZipMediaFiles(zr.File, ankiMedia, overwrite)
	if err != nil {
		return fmt.Errorf("copy media from %s: %w", zipPath, err)
	}
	if copied+skipped > 0 {
		fmt.Printf("media: copied=%d skipped=%d -> %s\n", copied, skipped, ankiMedia)
	}

	// item.csv / items.csv / 他CSV の順で1つ選択
	for _, f := range zr.File {
		base := strings.ToLower(path.Base(f.Name)) // zip 内は常に '/' 区切り
		switch base {
		case "item.csv", "items.csv":
			target = f
		default:
			if strings.HasSuffix(base, ".csv") && fallback == nil {
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

// ----- media/ 内のファイルを Anki の collection.media にコピー -----
func copyZipMediaFiles(files []*zip.File, dest string, overwrite bool) (copied, skipped int, err error) {
	for _, f := range files {
		name := strings.TrimLeft(f.Name, "/\\")
		parts := strings.Split(name, "/")
		if len(parts) == 0 || !strings.EqualFold(parts[0], "media") {
			continue
		}
		rel := strings.Join(parts[1:], "/")
		if rel == "" || f.FileInfo().IsDir() {
			continue
		}

		dstPath := filepath.Join(dest, filepath.FromSlash(rel))
		if !overwrite && fileExists(dstPath) {
			skipped++
			continue
		}
		check(os.MkdirAll(filepath.Dir(dstPath), 0o755))

		src, openErr := f.Open()
		if openErr != nil {
			return copied, skipped, openErr
		}
		func() {
			defer src.Close()
			tmp := dstPath + ".tmp~"
			out, createErr := os.Create(tmp)
			if createErr != nil {
				err = createErr
				return
			}
			if _, err = io.Copy(out, src); err == nil {
				err = out.Close()
			} else {
				out.Close()
			}
			if err == nil {
				err = os.Rename(tmp, dstPath)
			} else {
				_ = os.Remove(tmp)
			}
		}()
		if err != nil {
			return copied, skipped, err
		}
		copied++
	}
	return copied, skipped, nil
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

    // 2) 訳文抽出（優先度：div直書き → どこでも .dc-translation）
    ja := ""
    if m := reTransDiv.FindStringSubmatch(h); len(m) >= 2 {
        ja = strings.TrimSpace(stripTags(m[1]))
        // 元HTMLからは翻訳行を削除（1カラム目に出したくないため）
        h = reTransDiv.ReplaceAllString(h, "")
    }
    if ja == "" {
        if m := reAnyTranslation.FindStringSubmatch(h); len(m) >= 2 {
            ja = strings.TrimSpace(stripTags(m[1]))
            // Cloze 内でも後で置換されるが、二重表示を避けたいなら明示的に除去しても良い
            // h = reAnyTranslation.ReplaceAllString(h, "")
        }
    }

    // 3) Cloze → 前半のみ色付き化（後半＝訳文は 2) で ja に入れる）
    h = reCloze.ReplaceAllStringFunc(h, func(s string) string {
        g := reCloze.FindStringSubmatch(s)
        front := ""
        back  := ""
        if len(g) >= 2 { front = g[1] }
        if len(g) >= 3 { back  = g[2] }

        // Cloze 後半に訳文があり、まだ ja が空なら採用
        if ja == "" && strings.TrimSpace(back) != "" {
            ja = strings.TrimSpace(stripTags(back))
        }

        // 前半をプレーンテキスト化して色付け
        plain := strings.TrimSpace(stripTags(front))
        if plain == "" {
            return ""
        }
        return `<span style="color:` + color + `;">` + html.EscapeString(plain) + `</span>`
    })

    // 3.5) Cloze外に残る装飾を剥がす
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
        ss := s
        if !strings.HasPrefix(ss, "[sound:") {
            if i := strings.Index(ss, "[sound:"); i >= 0 {
                ss = ss[i:]
            }
        }
        if idx := strings.Index(h, `<div class="dc-line"`); idx >= 0 {
            if end := strings.Index(h[idx:], `</div>`); end >= 0 {
                insert := idx + end + len(`</div>`)
                audio := `<div class="dc-audio" style="padding:0.4rem;margin-top:0.25rem;">` + ss + `</div>`
                h = h[:insert] + audio + h[insert:]
            }
        }
    }

    // 軽い整形
    h = strings.ReplaceAll(h, "  ", " ")
    return h, ja
}


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

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}
