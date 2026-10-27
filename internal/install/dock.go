// dock.go: the pure XML transformation behind the macOS Dock update (PLAN
// 1a "Launcher"). The Dock's launcher list, persistent-apps, lives in the
// com.apple.dock preferences; shortcuts_darwin.go reads those through
// cfprefsd with `defaults export com.apple.dock -`, applies one of the
// functions here, writes the result back with `defaults import
// com.apple.dock -` and restarts the Dock. Everything in this file is pure:
// no filesystem, no tools, so the whole transform is unit-testable on any
// platform. Only the persistent-apps region is ever modified; the rest of
// the document round-trips byte for byte.
package install

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
)

// plistKind identifies one node of a parsed plist document.
type plistKind int

const (
	plistElement plistKind = iota
	plistCharData
	plistDirective
	plistComment
	plistProcInst
)

// plistNode is one node of a parsed property list XML document: an element
// with its attributes and children, or a piece of raw text (character data,
// the DOCTYPE directive, a comment, a processing instruction such as the
// <?xml?> prolog) kept verbatim, so an unmodified document round-trips byte
// for byte.
type plistNode struct {
	kind     plistKind
	name     string // element name (plistElement)
	attrs    []xml.Attr
	text     string // CharData, Directive, Comment or ProcInst payload
	children []*plistNode
}

// parsePlist parses a plist XML document into a node tree.
func parsePlist(r io.Reader) (*plistNode, error) {
	dec := xml.NewDecoder(r)
	root := &plistNode{}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return root, nil
		}
		if err != nil {
			return nil, err
		}
		if start, ok := tok.(xml.StartElement); ok {
			n, err := parsePlistElement(dec, start)
			if err != nil {
				return nil, err
			}
			root.children = append(root.children, n)
			continue
		}
		n, err := plistRawNode(tok)
		if err != nil {
			return nil, err
		}
		if n != nil {
			root.children = append(root.children, n)
		}
	}
}

// parsePlistElement parses one element and its subtree.
func parsePlistElement(dec *xml.Decoder, start xml.StartElement) (*plistNode, error) {
	n := &plistNode{kind: plistElement, name: start.Name.Local, attrs: start.Attr}
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		if childStart, ok := tok.(xml.StartElement); ok {
			child, err := parsePlistElement(dec, childStart)
			if err != nil {
				return nil, err
			}
			n.children = append(n.children, child)
			continue
		}
		if end, ok := tok.(xml.EndElement); ok {
			if end.Name.Local != start.Name.Local {
				return nil, fmt.Errorf("mismatched </%s> while parsing <%s>", end.Name.Local, start.Name.Local)
			}
			return n, nil
		}
		raw, err := plistRawNode(tok)
		if err != nil {
			return nil, err
		}
		if raw != nil {
			n.children = append(n.children, raw)
		}
	}
}

// plistRawNode turns a non-element token into a verbatim node; nil for
// tokens the element parser handles itself.
func plistRawNode(tok xml.Token) (*plistNode, error) {
	switch t := tok.(type) {
	case xml.CharData:
		return &plistNode{kind: plistCharData, text: string(t)}, nil
	case xml.Directive:
		return &plistNode{kind: plistDirective, text: string(t)}, nil
	case xml.Comment:
		return &plistNode{kind: plistComment, text: string(t)}, nil
	case xml.ProcInst:
		return &plistNode{kind: plistProcInst, text: t.Target + " " + string(t.Inst)}, nil
	}
	return nil, nil
}

// writeTo writes the node: nodes parsed from the document reproduce their
// source content (character data re-escaped, see escapeXMLText), and nodes
// built here are written the way they were built.
func (n *plistNode) writeTo(w io.Writer) error {
	switch n.kind {
	case plistCharData:
		_, err := io.WriteString(w, escapeXMLText(n.text))
		return err
	case plistDirective:
		_, err := fmt.Fprintf(w, "<!%s>", n.text)
		return err
	case plistComment:
		_, err := fmt.Fprintf(w, "<!--%s-->", n.text)
		return err
	case plistProcInst:
		_, err := fmt.Fprintf(w, "<?%s?>", n.text)
		return err
	case plistElement:
		return n.writeElement(w)
	}
	return nil
}

// escapeXMLText escapes the XML-significant characters of text parsed from
// the document. The decoder resolves entity references (AT&amp;T becomes
// AT&T), so the escaped form must be restored on write for the document to
// stay valid; generated text (file URLs are percent-encoded, labels and
// keys are fixed ASCII) is unaffected.
func escapeXMLText(s string) string {
	if !strings.ContainsAny(s, "&<>") {
		return s
	}
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// writeElement writes an element: a self-closing tag when it has no
// children (as plists write <true/> and <dict/>), the open tag, its
// children and the close tag otherwise. The children carry their own
// formatting, so untouched content is reproduced exactly (character data
// re-escaped, because the decoder resolved the document's entities).
func (n *plistNode) writeElement(w io.Writer) error {
	if len(n.children) == 0 {
		return writeSelfClosing(w, n)
	}
	if err := writeOpenTag(w, n); err != nil {
		return err
	}
	for _, c := range n.children {
		if err := c.writeTo(w); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "</%s>", n.name)
	return err
}

func writeOpenTag(w io.Writer, n *plistNode) error {
	if _, err := io.WriteString(w, "<"+n.name); err != nil {
		return err
	}
	for _, a := range n.attrs {
		if _, err := fmt.Fprintf(w, " %s=\"%s\"", a.Name.Local, escapeXMLText(a.Value)); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, ">")
	return err
}

func writeSelfClosing(w io.Writer, n *plistNode) error {
	if err := writeOpenTagWithoutClose(w, n); err != nil {
		return err
	}
	_, err := io.WriteString(w, "/>")
	return err
}

func writeOpenTagWithoutClose(w io.Writer, n *plistNode) error {
	if _, err := io.WriteString(w, "<"+n.name); err != nil {
		return err
	}
	for _, a := range n.attrs {
		if _, err := fmt.Fprintf(w, " %s=\"%s\"", a.Name.Local, escapeXMLText(a.Value)); err != nil {
			return err
		}
	}
	return nil
}

// plistWrite renders the document back to bytes.
func plistWrite(root *plistNode) []byte {
	var buf bytes.Buffer
	for _, c := range root.children {
		_ = c.writeTo(&buf)
	}
	return buf.Bytes()
}

// plistElem builds an element node with the given children.
func plistElem(name string, children ...*plistNode) *plistNode {
	return &plistNode{kind: plistElement, name: name, children: children}
}

// plistText builds a character data node.
func plistText(s string) *plistNode {
	return &plistNode{kind: plistCharData, text: s}
}

// elementText returns the text content of an element (its character data
// children joined).
func elementText(n *plistNode) string {
	var b strings.Builder
	for _, c := range n.children {
		if c.kind == plistCharData {
			b.WriteString(c.text)
		}
	}
	return b.String()
}

// plistRootDict returns the document's top-level <dict>.
func plistRootDict(root *plistNode) *plistNode {
	for _, c := range root.children {
		if c.kind == plistElement && c.name == "plist" {
			for _, d := range c.children {
				if d.kind == plistElement && d.name == "dict" {
					return d
				}
			}
		}
	}
	return nil
}

// dictValue returns the value element after <key>key</key> inside a plist
// dict, or nil. Whitespace between the key and its value is skipped.
func dictValue(dict *plistNode, key string) *plistNode {
	for i, c := range dict.children {
		if c.kind == plistElement && c.name == "key" && elementText(c) == key {
			for _, v := range dict.children[i+1:] {
				if v.kind == plistElement {
					return v
				}
			}
			return nil
		}
	}
	return nil
}

// persistentApps returns the root dict's <key>persistent-apps</key> element
// and its value element, nil when the key is absent. A value that is not an
// <array> is returned too, so the caller can reject it.
func persistentApps(dict *plistNode) (key, value *plistNode) {
	for i, c := range dict.children {
		if c.kind == plistElement && c.name == "key" && elementText(c) == "persistent-apps" {
			for _, v := range dict.children[i+1:] {
				if v.kind == plistElement {
					return c, v
				}
			}
			return c, nil
		}
	}
	return nil, nil
}

// dictLineIndent returns the indentation of a dict's entries (the run of
// spaces and tabs after the newline that follows the <dict> open tag), or
// "\t" when the export is not newline-formatted.
func dictLineIndent(dict *plistNode) string {
	for _, c := range dict.children {
		if c.kind == plistCharData && c.text != "" {
			if j := strings.LastIndex(c.text, "\n"); j >= 0 {
				return c.text[j+1:]
			}
		}
		break
	}
	return "\t"
}

// normalizeDockURL reduces a tile URL to a comparable form: the decoded
// path with any trailing slash trimmed, because macOS writes app URLs with
// a trailing slash (file:///Applications/Finder.app/) and the same app may
// appear without one.
func normalizeDockURL(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err == nil && u.Scheme == "file" && (u.Host == "" || u.Host == "localhost") {
		return strings.TrimSuffix(u.Path, "/")
	}
	return strings.TrimSuffix(strings.TrimSpace(s), "/")
}

// tileURL returns the app URL of one persistent-apps tile, normalized, or
// "" for tiles that do not name an app (separators, spaces, recents):
// modern tiles keep it under tile-data/file-data/_CFURLString, a few old
// ones put the URL string directly in file-data.
func tileURL(tile *plistNode) string {
	if tile == nil || tile.name != "dict" {
		return ""
	}
	fileData := dictValue(dictValue(tile, "tile-data"), "file-data")
	if fileData == nil {
		return ""
	}
	if fileData.name == "string" {
		return normalizeDockURL(elementText(fileData))
	}
	if v := dictValue(fileData, "_CFURLString"); v != nil && v.name == "string" {
		return normalizeDockURL(elementText(v))
	}
	return ""
}

// dockHasEntry reports whether the persistent-apps array already holds a
// tile for any of urls. Both entry bundle locations are passed, so a Dock
// pinned before /Applications became the default is recognised.
func dockHasEntry(apps *plistNode, urls []string) bool {
	if apps == nil {
		return false
	}
	want := make(map[string]bool, len(urls))
	for _, u := range urls {
		want[normalizeDockURL(u)] = true
	}
	for _, c := range apps.children {
		if c.kind == plistElement && c.name == "dict" && want[tileURL(c)] {
			return true
		}
	}
	return false
}

// plistKVPair returns the formatted nodes of one <key>/value pair inside a
// dict whose key and value lines sit at indent.
func plistKVPair(indent, key string, value *plistNode) []*plistNode {
	return []*plistNode{
		plistText("\n" + indent),
		plistElem("key", plistText(key)),
		plistText("\n" + indent),
		value,
	}
}

// plistDict builds a <dict> holding plistKVPair pairs: each key and value
// on its own line at the pair indent, and the closing tag on its own line
// at indent (the dict's own line indent).
func plistDict(indent string, pairs ...[]*plistNode) *plistNode {
	var children []*plistNode
	for _, kv := range pairs {
		children = append(children, kv...)
	}
	children = append(children, plistText("\n"+indent))
	return plistElem("dict", children...)
}

// dockTile builds the plist tile for the entry bundle: a file-tile dict
// whose file-data names appURL with _CFURLStringType 15 (the absolute file
// URL form the Dock writes), plus the metadata real tiles carry, formatted
// like the rest of the export with indent as the tile's own line indent.
// All text is ASCII (the URL is percent-encoded, the rest is fixed), so no
// XML escaping is needed when the tile is written.
func dockTile(appURL string, guid int64, indent string) *plistNode {
	kv1 := indent + "\t"     // the tile's key/value line indent
	kv2 := indent + "\t\t"   // tile-data's line indent
	kv3 := indent + "\t\t\t" // file-data's line indent
	fileData := plistDict(indent+"\t\t",
		plistKVPair(kv3, "_CFURLString", plistElem("string", plistText(appURL))),
		plistKVPair(kv3, "_CFURLStringType", plistElem("integer", plistText("15"))),
	)
	tileData := plistDict(indent+"\t",
		plistKVPair(kv2, "file-data", fileData),
		plistKVPair(kv2, "file-label", plistElem("string", plistText("kids-shelf"))),
		plistKVPair(kv2, "file-type", plistElem("integer", plistText("41"))),
		plistKVPair(kv2, "dock-extra", plistElem("false")),
		plistKVPair(kv2, "is-beta", plistElem("false")),
	)
	return plistDict(indent,
		plistKVPair(kv1, "GUID", plistElem("integer", plistText(fmt.Sprintf("%d", guid)))),
		plistKVPair(kv1, "tile-data", tileData),
		plistKVPair(kv1, "tile-type", plistElem("string", plistText("file-tile"))),
	)
}

// appendTile appends tile to a persistent-apps array, using tileIndent for
// the new tile's line and keeping the array's own trailing whitespace (or a
// fresh closing line when the array had none).
func appendTile(apps *plistNode, tile *plistNode, tileIndent string) {
	trailing := ""
	if len(apps.children) > 0 && apps.children[len(apps.children)-1].kind == plistCharData {
		trailing = apps.children[len(apps.children)-1].text
		apps.children = apps.children[:len(apps.children)-1]
	}
	apps.children = append(apps.children,
		&plistNode{kind: plistCharData, text: "\n" + tileIndent},
		tile,
	)
	if trailing == "" {
		trailing = "\n" + strings.TrimSuffix(tileIndent, "\t")
	}
	apps.children = append(apps.children, &plistNode{kind: plistCharData, text: trailing})
}

// dockPlistAddEntry returns the com.apple.dock export with a tile for the
// entry bundle appended to persistent-apps, unless a tile already holds one
// of urls (idempotent; the check recognises both entry bundle locations).
// appURL is the tile URL to add, guid the new tile's GUID. changed reports
// whether the returned document differs from doc. Everything outside the
// persistent-apps array is preserved byte for byte.
func dockPlistAddEntry(doc []byte, appURL string, urls []string, guid int64) (out []byte, changed bool, err error) {
	root, err := parsePlist(bytes.NewReader(doc))
	if err != nil {
		return nil, false, fmt.Errorf("parsing the Dock preferences: %w", err)
	}
	dict := plistRootDict(root)
	if dict == nil {
		return nil, false, errors.New("the Dock preferences have no plist root dict")
	}
	_, value := persistentApps(dict)
	if value != nil && value.name != "array" {
		return nil, false, fmt.Errorf("persistent-apps is a <%s>, not an <array>", value.name)
	}
	if dockHasEntry(value, urls) {
		return doc, false, nil
	}
	indent := dictLineIndent(dict) + "\t"
	tile := dockTile(appURL, guid, indent)
	if value == nil {
		// The Dock has no persistent-apps list yet: add the key and a
		// fresh array holding the tile, before the dict's closing tag.
		pair := []*plistNode{
			plistText("\n" + dictLineIndent(dict)),
			plistElem("key", plistText("persistent-apps")),
			plistText("\n" + dictLineIndent(dict)),
			plistElem("array",
				plistText("\n"+indent),
				tile,
				plistText("\n"+dictLineIndent(dict)),
			),
		}
		idx := len(dict.children)
		if len(dict.children) > 0 && dict.children[len(dict.children)-1].kind == plistCharData {
			idx = len(dict.children) - 1
		}
		dict.children = append(dict.children[:idx], append(pair, dict.children[idx:]...)...)
	} else {
		appendTile(value, tile, indent)
	}
	return plistWrite(root), true, nil
}

// dockPlistRemoveEntry returns the com.apple.dock export with every
// persistent-apps tile whose URL is one of urls removed; other tiles and
// everything outside the array are preserved byte for byte. changed reports
// whether the returned document differs from doc.
func dockPlistRemoveEntry(doc []byte, urls []string) (out []byte, changed bool, err error) {
	root, err := parsePlist(bytes.NewReader(doc))
	if err != nil {
		return nil, false, fmt.Errorf("parsing the Dock preferences: %w", err)
	}
	dict := plistRootDict(root)
	if dict == nil {
		return nil, false, errors.New("the Dock preferences have no plist root dict")
	}
	_, value := persistentApps(dict)
	if value == nil {
		return doc, false, nil
	}
	if value.name != "array" {
		return nil, false, fmt.Errorf("persistent-apps is a <%s>, not an <array>", value.name)
	}
	want := make(map[string]bool, len(urls))
	for _, u := range urls {
		want[normalizeDockURL(u)] = true
	}
	kept := make([]*plistNode, 0, len(value.children))
	removed := false
	for _, c := range value.children {
		if c.kind == plistElement && c.name == "dict" && want[tileURL(c)] {
			if len(kept) > 0 && kept[len(kept)-1].kind == plistCharData {
				kept = kept[:len(kept)-1] // drop the removed tile's line
			}
			removed = true
			continue
		}
		kept = append(kept, c)
	}
	if !removed {
		return doc, false, nil
	}
	value.children = kept
	return plistWrite(root), true, nil
}
