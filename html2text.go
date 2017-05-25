package html2text

import (
	"bytes"
	"io"
	"regexp"
	"strings"
	"unicode"

	"github.com/olekukonko/tablewriter"
	"github.com/ssor/bom"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// FromHtmlNode renders text output from a pre-parsed HTML document.
func FromHtmlNode(doc *html.Node) (string, error) {
	ctx := textifyTraverseContext{
		buf: bytes.Buffer{},
	}
	if err := ctx.traverse(doc); err != nil {
		return "", err
	}

	text := strings.TrimSpace(newlineRe.ReplaceAllString(
		strings.Replace(ctx.buf.String(), "\n ", "\n", -1), "\n\n"),
	)
	return text, nil
}

// FromReaders renders text output after parsing HTML for the specified
// io.Reader.
func FromReader(reader io.Reader) (string, error) {
	newReader, err := bom.NewReaderWithoutBom(reader)
	if err != nil {
		return "", err
	}
	doc, err := html.Parse(newReader)
	if err != nil {
		return "", err
	}
	return FromHtmlNode(doc)
}

// FromString parses HTML from the input string, then renders the text form.
func FromString(input string) (string, error) {
	bs := bom.CleanBom([]byte(input))
	text, err := FromReader(bytes.NewReader(bs))
	if err != nil {
		return "", err
	}
	return text, nil
}

var (
	spacingRe = regexp.MustCompile(`[ \r\n\t]+`)
	newlineRe = regexp.MustCompile(`\n\n+`)
)

// traverseTableCtx holds text-related context.
type textifyTraverseContext struct {
	buf bytes.Buffer

	prefix          string
	blockquoteLevel int
	lineLength      int
	endsWithSpace   bool
	endsWithNewline bool
	justClosedDiv   bool
	tableCtx        tableTraverseContext
}

// tableTraverseContext holds table ASCII-form related context.
type tableTraverseContext struct {
	header     []string
	body       [][]string
	footer     []string
	tmpRow     int
	isInFooter bool
}

func (ctx *textifyTraverseContext) traverse(node *html.Node) error {
	switch node.Type {
	default:
		return ctx.traverseChildren(node)

	case html.TextNode:
		data := strings.Trim(spacingRe.ReplaceAllString(node.Data, " "), " ")
		return ctx.emit(data)

	case html.ElementNode:
		return ctx.handleElementNode(node)
	}
}

func (ctx *textifyTraverseContext) handleElementNode(node *html.Node) error {
	ctx.justClosedDiv = false
	switch node.DataAtom {
	case atom.Br:
		return ctx.emit("\n")

	case atom.H1, atom.H2, atom.H3:
		subCtx := textifyTraverseContext{}
		if err := subCtx.traverseChildren(node); err != nil {
			return err
		}

		str := subCtx.buf.String()
		dividerLen := 0
		for _, line := range strings.Split(str, "\n") {
			if lineLen := len([]rune(line)); lineLen-1 > dividerLen {
				dividerLen = lineLen - 1
			}
		}
		divider := ""
		if node.DataAtom == atom.H1 {
			divider = strings.Repeat("*", dividerLen)
		} else {
			divider = strings.Repeat("-", dividerLen)
		}

		if node.DataAtom == atom.H3 {
			return ctx.emit("\n\n" + str + "\n" + divider + "\n\n")
		}
		return ctx.emit("\n\n" + divider + "\n" + str + "\n" + divider + "\n\n")

	case atom.Blockquote:
		ctx.blockquoteLevel++
		ctx.prefix = strings.Repeat(">", ctx.blockquoteLevel) + " "
		if err := ctx.emit("\n"); err != nil {
			return err
		}
		if ctx.blockquoteLevel == 1 {
			if err := ctx.emit("\n"); err != nil {
				return err
			}
		}
		if err := ctx.traverseChildren(node); err != nil {
			return err
		}
		ctx.blockquoteLevel--
		ctx.prefix = strings.Repeat(">", ctx.blockquoteLevel)
		if ctx.blockquoteLevel > 0 {
			ctx.prefix += " "
		}
		return ctx.emit("\n\n")

	case atom.Div:
		if ctx.lineLength > 0 {
			if err := ctx.emit("\n"); err != nil {
				return err
			}
		}
		if err := ctx.traverseChildren(node); err != nil {
			return err
		}
		var err error
		if ctx.justClosedDiv == false {
			err = ctx.emit("\n")
		}
		ctx.justClosedDiv = true
		return err

	case atom.Li:
		if err := ctx.emit("* "); err != nil {
			return err
		}

		if err := ctx.traverseChildren(node); err != nil {
			return err
		}

		return ctx.emit("\n")

	case atom.B, atom.Strong:
		subCtx := textifyTraverseContext{}
		subCtx.endsWithSpace = true
		if err := subCtx.traverseChildren(node); err != nil {
			return err
		}
		str := subCtx.buf.String()
		return ctx.emit("*" + str + "*")

	case atom.A:
		// If image is the only child, take its alt text as the link text.
		if img := node.FirstChild; img != nil && node.LastChild == img && img.DataAtom == atom.Img {
			if altText := getAttrVal(img, "alt"); altText != "" {
				ctx.emit(altText)
			}
		} else if err := ctx.traverseChildren(node); err != nil {
			return err
		}

		hrefLink := ""
		if attrVal := getAttrVal(node, "href"); attrVal != "" {
			attrVal = ctx.normalizeHrefLink(attrVal)
			if attrVal != "" {
				hrefLink = "( " + attrVal + " )"
			}
		}

		return ctx.emit(hrefLink)

	case atom.P, atom.Ul:
		if err := ctx.emit("\n\n"); err != nil {
			return err
		}

		if err := ctx.traverseChildren(node); err != nil {
			return err
		}

		return ctx.emit("\n\n")

	case atom.Table:
		if err := ctx.emit("\n\n"); err != nil {
			return err
		}
		// Re-intialize all table context.
		ctx.tableCtx.body = [][]string{}
		ctx.tableCtx.header = []string{}
		ctx.tableCtx.footer = []string{}
		ctx.tableCtx.isInFooter = false
		ctx.tableCtx.tmpRow = 0

		// Browse children, enriching context with table data.
		if err := ctx.traverseChildren(node); err != nil {
			return err
		}

		buf := new(bytes.Buffer)
		table := tablewriter.NewWriter(buf)
		table.SetHeader(ctx.tableCtx.header)
		table.SetFooter(ctx.tableCtx.footer)
		table.AppendBulk(ctx.tableCtx.body)

		// Render the table using ASCII.
		table.Render()
		if err := ctx.emit(buf.String()); err != nil {
			return err
		}

		return ctx.emit("\n\n")

	case atom.Tfoot:
		ctx.tableCtx.isInFooter = true
		if err := ctx.traverseChildren(node); err != nil {
			return err
		}
		ctx.tableCtx.isInFooter = false

		return nil

	case atom.Tr:
		ctx.tableCtx.body = append(ctx.tableCtx.body, []string{})
		if err := ctx.traverseChildren(node); err != nil {
			return err
		}
		ctx.tableCtx.tmpRow++

		return nil

	case atom.Th:
		res, err := getContentAsString(node)
		if err != nil {
			return err
		}

		ctx.tableCtx.header = append(ctx.tableCtx.header, res)

		return nil

	case atom.Td:
		res, err := getContentAsString(node)
		if err != nil {
			return err
		}

		if ctx.tableCtx.isInFooter {
			ctx.tableCtx.footer = append(ctx.tableCtx.footer, res)
		} else {
			ctx.tableCtx.body[ctx.tableCtx.tmpRow] = append(ctx.tableCtx.body[ctx.tableCtx.tmpRow], res)
		}

		return nil

	case atom.Style, atom.Script, atom.Head:
		// Ignore the subtree.
		return nil

	default:
		return ctx.traverseChildren(node)
	}
}

func (ctx *textifyTraverseContext) traverseChildren(node *html.Node) error {
	for c := node.FirstChild; c != nil; c = c.NextSibling {
		if err := ctx.traverse(c); err != nil {
			return err
		}
	}

	return nil
}

func (ctx *textifyTraverseContext) emit(data string) error {
	if data == "" {
		return nil
	}
	var (
		lines = ctx.breakLongLines(data)
		err   error
	)
	for _, line := range lines {
		runes := []rune(line)
		startsWithSpace := unicode.IsSpace(runes[0])
		if !startsWithSpace && !ctx.endsWithSpace {
			if err = ctx.buf.WriteByte(' '); err != nil {
				return err
			}
			ctx.lineLength++
		}
		ctx.endsWithSpace = unicode.IsSpace(runes[len(runes)-1])
		for _, c := range line {
			if _, err = ctx.buf.WriteString(string(c)); err != nil {
				return err
			}
			ctx.lineLength++
			if c == '\n' {
				ctx.lineLength = 0
				if ctx.prefix != "" {
					if _, err = ctx.buf.WriteString(ctx.prefix); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func (ctx *textifyTraverseContext) breakLongLines(data string) []string {
	// Only break lines when in blockquotes.
	if ctx.blockquoteLevel == 0 {
		return []string{data}
	}
	var (
		ret      = []string{}
		runes    = []rune(data)
		l        = len(runes)
		existing = ctx.lineLength
	)
	if existing >= 74 {
		ret = append(ret, "\n")
		existing = 0
	}
	for l+existing > 74 {
		i := 74 - existing
		for i >= 0 && !unicode.IsSpace(runes[i]) {
			i--
		}
		if i == -1 {
			// No spaces, so go the other way.
			i = 74 - existing
			for i < l && !unicode.IsSpace(runes[i]) {
				i++
			}
		}
		ret = append(ret, string(runes[:i])+"\n")
		for i < l && unicode.IsSpace(runes[i]) {
			i++
		}
		runes = runes[i:]
		l = len(runes)
		existing = 0
	}
	if len(runes) > 0 {
		ret = append(ret, string(runes))
	}
	return ret
}

func (ctx *textifyTraverseContext) normalizeHrefLink(link string) string {
	link = strings.TrimSpace(link)
	link = strings.TrimPrefix(link, "mailto:")
	return link
}

func getAttrVal(node *html.Node, attrName string) string {
	for _, attr := range node.Attr {
		if attr.Key == attrName {
			return attr.Val
		}
	}

	return ""
}

// getContentAsString browse every child of node and get content as string
func getContentAsString(node *html.Node) (string, error) {
	var res string
	for c := node.FirstChild; c != nil; c = c.NextSibling {
		s, err := FromHtmlNode(c)
		if err != nil {
			return "", err
		}
		res += s
		if c.NextSibling != nil {
			res += "\n"
		}
	}
	return res, nil
}
