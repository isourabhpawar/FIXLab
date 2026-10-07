// Package dictionary loads the standard FIX data dictionaries at startup
// and exposes tag → name/type/enumeration lookups (spec §17).
//
// The canonical XML files live in backend/specs and are embedded into the
// binary so the server has no runtime file dependency.
package dictionary

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	fixspecs "fixlab.dev/fixlab/backend/specs"
)

// FieldDef describes a single FIX field: its name, data type, and any
// enumerated values (value → description).
type FieldDef struct {
	Tag   int
	Name  string
	Type  string
	Enums map[string]string
}

// Dictionary is the parsed view of one FIX data dictionary (e.g. FIX44).
type Dictionary struct {
	BeginString string
	Fields      map[int]FieldDef
	// MsgTypeToName maps 35= values (e.g. "D") to message names
	// (e.g. "NewOrderSingle").
	MsgTypeToName map[string]string
}

type xmlValue struct {
	Enum        string `xml:"enum,attr"`
	Description string `xml:"description,attr"`
}

type xmlField struct {
	Number string     `xml:"number,attr"`
	Name   string     `xml:"name,attr"`
	Type   string     `xml:"type,attr"`
	Values []xmlValue `xml:"value"`
}

type xmlMessage struct {
	Name    string `xml:"name,attr"`
	MsgType string `xml:"msgtype,attr"`
}

type xmlFix struct {
	// XMLName is untagged on purpose: a non-<fix> root must unmarshal
	// cleanly so ParseBytes can reject it with a clear "not a FIX data
	// dictionary" error instead of an XML syntax error.
	XMLName  xml.Name
	Major    string       `xml:"major,attr"`
	Minor    string       `xml:"minor,attr"`
	Type     string       `xml:"type,attr"`
	Fields   []xmlField   `xml:"fields>field"`
	Messages []xmlMessage `xml:"messages>message"`
}

// ParseBytes parses raw QuickFIX data-dictionary XML into a Dictionary
// (phase 2.4: custom dictionaries). Validation is strict and every
// failure names the exact problem, because the input is user-supplied:
//
//   - the XML must be well-formed;
//   - the root element must be <fix> (a QuickFIX data dictionary);
//   - major/minor version attributes (or a type) must be present so a
//     BeginString can be derived;
//   - <fields> must contain at least one <field>, each with a numeric
//     number and a non-empty name.
//
// Enum values (<value enum=".." description="..">) are carried into the
// field definitions; messages are counted and mapped msgType → name.
func ParseBytes(data []byte) (*Dictionary, error) {
	var doc xmlFix
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("dictionary: malformed XML: %v", err)
	}
	if doc.XMLName.Local != "fix" {
		return nil, fmt.Errorf("dictionary: not a FIX data dictionary: root element is <%s>, want <fix>", doc.XMLName.Local)
	}
	major, minor := strings.TrimSpace(doc.Major), strings.TrimSpace(doc.Minor)
	if major == "" || minor == "" {
		return nil, fmt.Errorf("dictionary: <fix> needs major/minor version attributes (e.g. major=\"4\" minor=\"4\") to derive the BeginString")
	}
	beginString := "FIX." + major + "." + minor
	if strings.EqualFold(strings.TrimSpace(doc.Type), "FIXT") {
		beginString = "FIXT." + major + "." + minor
	}
	if len(doc.Fields) == 0 {
		return nil, fmt.Errorf("dictionary: no <field> definitions found in <fields>")
	}
	d := &Dictionary{
		BeginString:   beginString,
		Fields:        make(map[int]FieldDef, len(doc.Fields)),
		MsgTypeToName: make(map[string]string, len(doc.Messages)),
	}
	for i, f := range doc.Fields {
		tag, err := strconv.Atoi(strings.TrimSpace(f.Number))
		if err != nil || tag <= 0 {
			return nil, fmt.Errorf("dictionary: field #%d has invalid number %q: tags are positive integers", i+1, f.Number)
		}
		name := strings.TrimSpace(f.Name)
		if name == "" {
			return nil, fmt.Errorf("dictionary: field #%d (tag %d) has no name", i+1, tag)
		}
		if _, dup := d.Fields[tag]; dup {
			return nil, fmt.Errorf("dictionary: duplicate definition for tag %d (%q)", tag, name)
		}
		def := FieldDef{Tag: tag, Name: name, Type: strings.TrimSpace(f.Type), Enums: map[string]string{}}
		for _, v := range f.Values {
			def.Enums[v.Enum] = v.Description
		}
		d.Fields[tag] = def
	}
	for _, m := range doc.Messages {
		if m.MsgType != "" {
			d.MsgTypeToName[m.MsgType] = m.Name
		}
	}
	return d, nil
}

var (
	loaded     = map[string]*Dictionary{}
	loadedLock sync.RWMutex
)

// Load parses and caches the dictionary for the given file name
// (e.g. "FIX44.xml"). Dictionaries are loaded once and shared.
func Load(fileName string) (*Dictionary, error) {
	loadedLock.RLock()
	if d, ok := loaded[fileName]; ok {
		loadedLock.RUnlock()
		return d, nil
	}
	loadedLock.RUnlock()

	raw, err := fixspecs.ReadFile(fileName)
	if err != nil {
		return nil, fmt.Errorf("dictionary: read embedded spec %s: %w", fileName, err)
	}
	d, err := ParseBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("dictionary: parse %s: %w", fileName, err)
	}

	loadedLock.Lock()
	loaded[fileName] = d
	loadedLock.Unlock()
	return d, nil
}

// beginStringToFile maps a FIX BeginString (8=) value to its embedded
// dictionary file (phase 6: the stateless FIX decoder).
var beginStringToFile = map[string]string{
	"FIX.4.0":    "FIX40.xml",
	"FIX.4.2":    "FIX42.xml",
	"FIX.4.4":    "FIX44.xml",
	"FIXT.1.1":   "FIXT11.xml",
	"FIX.5.0":    "FIX50SP2.xml",
	"FIX.5.0SP2": "FIX50SP2.xml",
}

// SupportedBeginStrings lists the 8= values ForBeginString accepts.
func SupportedBeginStrings() []string {
	return []string{"FIX.4.0", "FIX.4.2", "FIX.4.4", "FIXT.1.1", "FIX.5.0SP2"}
}

// ForBeginString loads the embedded dictionary matching a FIX BeginString
// (8=) value, e.g. "FIX.4.4" → FIX44.xml. The error names the supported
// values for anything unmapped.
func ForBeginString(beginString string) (*Dictionary, error) {
	file, ok := beginStringToFile[beginString]
	if !ok {
		return nil, fmt.Errorf("dictionary: unsupported BeginString %q (supported: %s)",
			beginString, strings.Join(SupportedBeginStrings(), ", "))
	}
	return Load(file)
}

// SpecFileBytes returns the raw embedded XML for a spec file, so callers
// (e.g. the FIX engine) can materialize it as a real file when a library
// requires a filesystem path.
func SpecFileBytes(fileName string) ([]byte, error) {
	return fixspecs.ReadFile(fileName)
}

// MaterializeTemp writes all embedded dictionaries to a fresh temp
// directory and returns its path. QuickFIX/Go requires real files for
// DataDictionary, so the engine uses this at startup.
func MaterializeTemp() (string, error) {
	dir, err := os.MkdirTemp("", "fixlab-specs-")
	if err != nil {
		return "", fmt.Errorf("dictionary: temp dir: %w", err)
	}
	for _, name := range fixspecs.Names() {
		raw, err := fixspecs.ReadFile(name)
		if err != nil {
			os.RemoveAll(dir)
			return "", fmt.Errorf("dictionary: read %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
			os.RemoveAll(dir)
			return "", fmt.Errorf("dictionary: write %s: %w", name, err)
		}
	}
	return dir, nil
}

// Field returns the definition for a tag, or false if unknown.
func (d *Dictionary) Field(tag int) (FieldDef, bool) {
	f, ok := d.Fields[tag]
	return f, ok
}

// FieldName returns the human name for a tag, or "" if unknown.
func (d *Dictionary) FieldName(tag int) string {
	if f, ok := d.Fields[tag]; ok {
		return f.Name
	}
	return ""
}

// MessageName maps a 35= value to its message name (e.g. "D" →
// "NewOrderSingle"), or "" if unknown.
func (d *Dictionary) MessageName(msgType string) string {
	return d.MsgTypeToName[msgType]
}

// EnumDescription returns the description for an enumerated field value
// (e.g. tag 54 value "1" → "BUY"), or "" if none is defined.
func (d *Dictionary) EnumDescription(tag int, value string) string {
	if f, ok := d.Fields[tag]; ok {
		return f.Enums[value]
	}
	return ""
}
