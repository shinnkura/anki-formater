package main

import (
	"bufio"
	"encoding/csv"
	"flag"
	"fmt"
	"html"
	"io"
	"os"
	"regexp"
	"strings"
)

var (
	reStyleBlock = regexp.MustCompile(`(?is)<style.*?>.*?</style>`)
	reTransDiv   = regexp.MustCompile(`(?is)<div\s+class="dc-line\s+dc-translation[^"]*"\s*>(.*?)</div>`)
	// {{c1::...}} / {{c2::...::hint}} を検出
	reCloze = regexp.MustCompile(`(?is)\{\{c\d+::(.*?)(?:::[^}]*)?\}\}`)

	reImage   = regexp.MustCompile(`(?is)<div\s+class="([^"]*\bdc-image[^"]*)"\s+style="([^"]*?)"\s*></div>`)
	reBgImage = regexp.MustCompile(`(?is)background-image\s*:\s*url\(([^)]+)\)`)
	reCardDiv = regexp.MustCompile(`(?is)<div\s+class="([^"]*\bdc-card[^"]*)"(.*?)>`)

	// dc-down / dc-gap を“外す”（Cloze外に残っている場合に備えて）
	reUnwrapDown = regexp.MustCompile(`(?is)<span[^>]*class="[^"]*\bdc-down\b[^"]*"[^>]*>(.*?)</span>`)
	reUnwrapGap  = regexp.MustCompile(`(?is)<span[^>]*class="[^"]*\bdc-gap\b[^"]*"[^>]*>(.*?)</span>`)
)

func main() {
	in := flag.String("in", "items.csv", "input TSV file (3 cols: html, sound, tags)")
	out := flag.String("out", "items_out.tsv", "output TSV file (2 cols: html, translation)")
	color := flag.String("color", "#1569C7", "color for cloze terms (e.g. #e91e63 or red)")
	flag.Parse()

	inFile, err := os.Open(*in)
	check(err)
	defer inFile.Close()

	r := csv.NewReader(bufio.NewReader(inFile))
	r.Comma = '\t'
	r.LazyQuotes = true

	outFile, err := os.Create(*out)
	check(err)
	defer outFile.Close()

	w := csv.NewWriter(outFile)
	w.Comma = '\t'

	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		check(err)
		if len(rec) == 0 {
			continue
		}

		htmlIn := rec[0]
		sound := ""
		if len(rec) >= 2 {
			sound = strings.TrimSpace(rec[1])
		}

		htmlOut, ja := transform(htmlIn, sound, *color)
		check(w.Write([]string{htmlOut, ja}))
	}
	w.Flush()
	check(w.Error())

	fmt.Printf("done: %s\n", *out)
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
		plain := strings.TrimSpace(stripTags(raw)) // ★タグ除去
		if plain == "" {
			return "" //万一中身が空なら消す
		}
		// プレーン文字列は最小限だけエスケープ（< > & など）
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

func check(err error) {
	if err != nil {
		panic(err)
	}
}
