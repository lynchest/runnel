package main

import (
	"bytes"
	"io"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

var spaces = regexp.MustCompile(`[\t\n\f\r ]+`)

var skippedTags = map[string]bool{
	"script": true, "style": true, "svg": true, "nav": true, "footer": true,
	"noscript": true, "template": true, "form": true,
}

var blockTags = map[string]bool{
	"article": true, "aside": true, "blockquote": true, "div": true,
	"header": true, "main": true, "p": true, "section": true, "table": true, "tr": true,
}

func htmlToMarkdown(body []byte, contentType string) ([]byte, error) {
	reader, err := charset.NewReader(bytes.NewReader(body), contentType)
	if err != nil {
		return nil, err
	}
	tokenizer := html.NewTokenizer(reader)
	var out strings.Builder
	skippedDepth := 0
	inPre := false

	for {
		tokenType := tokenizer.Next()
		switch tokenType {
		case html.ErrorToken:
			if err := tokenizer.Err(); err != io.EOF {
				return nil, err
			}
			return []byte(normalizeMarkdown(out.String())), nil
		case html.StartTagToken:
			token := tokenizer.Token()
			tag := token.Data
			if skippedDepth > 0 {
				if skippedTags[tag] {
					skippedDepth++
				}
				continue
			}
			switch {
			case skippedTags[tag]:
				skippedDepth = 1
			case len(tag) == 2 && tag[0] == 'h' && tag[1] >= '1' && tag[1] <= '6':
				out.WriteString("\n\n" + strings.Repeat("#", int(tag[1]-'0')) + " ")
			case tag == "li":
				if out.Len() == 0 || !strings.HasSuffix(out.String(), "\n") {
					out.WriteByte('\n')
				}
				out.WriteString("- ")
			case tag == "br":
				out.WriteByte('\n')
			case tag == "pre":
				inPre = true
				out.WriteString("\n\n```\n")
			case blockTags[tag]:
				out.WriteString("\n\n")
			}
		case html.SelfClosingTagToken:
			if tokenizer.Token().Data == "br" && skippedDepth == 0 {
				out.WriteByte('\n')
			}
		case html.EndTagToken:
			tag := tokenizer.Token().Data
			if skippedDepth > 0 {
				if skippedTags[tag] {
					skippedDepth--
				}
				continue
			}
			if tag == "pre" {
				inPre = false
				out.WriteString("\n```\n")
			} else if blockTags[tag] || tag == "li" || (len(tag) == 2 && tag[0] == 'h') {
				out.WriteByte('\n')
			}
		case html.TextToken:
			if skippedDepth > 0 {
				continue
			}
			text := string(tokenizer.Text())
			if inPre {
				out.WriteString(text)
			} else if strings.TrimSpace(text) != "" {
				out.WriteString(spaces.ReplaceAllString(text, " "))
			}
		}
	}
}

func normalizeMarkdown(text string) string {
	lines := strings.Split(text, "\n")
	inCode := false
	for i, line := range lines {
		if strings.TrimSpace(line) == "```" {
			inCode = !inCode
			lines[i] = "```"
		} else if inCode {
			lines[i] = strings.TrimRight(line, " \t")
		} else {
			lines[i] = strings.TrimSpace(spaces.ReplaceAllString(line, " "))
		}
	}
	result := strings.TrimSpace(strings.Join(lines, "\n"))
	for strings.Contains(result, "\n\n\n") {
		result = strings.ReplaceAll(result, "\n\n\n", "\n\n")
	}
	return result + "\n"
}
