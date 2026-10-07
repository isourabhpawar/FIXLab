package dictionary

import (
	"strings"
	"testing"
)

func TestLoad_FIX44(t *testing.T) {
	d, err := Load("FIX44.xml")
	if err != nil {
		t.Fatal(err)
	}
	if d.BeginString != "FIX.4.4" {
		t.Fatalf("BeginString = %q, want FIX.4.4", d.BeginString)
	}
	if len(d.Fields) < 500 {
		t.Fatalf("expected hundreds of fields, got %d", len(d.Fields))
	}
}

func TestFieldLookup(t *testing.T) {
	d, _ := Load("FIX44.xml")
	f, ok := d.Field(35)
	if !ok {
		t.Fatal("tag 35 not found")
	}
	if f.Name != "MsgType" {
		t.Fatalf("tag 35 name = %q, want MsgType", f.Name)
	}
	if _, ok := d.Field(999999); ok {
		t.Fatal("unknown tag 999999 unexpectedly found")
	}
	if name := d.FieldName(11); name != "ClOrdID" {
		t.Fatalf("tag 11 name = %q, want ClOrdID", name)
	}
}

func TestEnumDescription(t *testing.T) {
	d, _ := Load("FIX44.xml")
	if got := d.EnumDescription(54, "1"); got != "BUY" {
		t.Fatalf("tag 54 value 1 = %q, want BUY", got)
	}
	if got := d.EnumDescription(54, "2"); got != "SELL" {
		t.Fatalf("tag 54 value 2 = %q, want SELL", got)
	}
	if got := d.EnumDescription(54, "99"); got != "" {
		t.Fatalf("tag 54 value 99 = %q, want empty", got)
	}
}

func TestMessageName(t *testing.T) {
	d, _ := Load("FIX44.xml")
	if got := d.MessageName("D"); got != "NewOrderSingle" {
		t.Fatalf("msgtype D = %q, want NewOrderSingle", got)
	}
	if got := d.MessageName("8"); got != "ExecutionReport" {
		t.Fatalf("msgtype 8 = %q, want ExecutionReport", got)
	}
	if got := d.MessageName("ZZ"); got != "" {
		t.Fatalf("msgtype ZZ = %q, want empty", got)
	}
}

func TestLoad_Cached(t *testing.T) {
	a, _ := Load("FIX44.xml")
	b, _ := Load("FIX44.xml")
	if a != b {
		t.Fatal("expected cached dictionary instance")
	}
}

func TestLoad_Missing(t *testing.T) {
	if _, err := Load("NOPE.xml"); err == nil {
		t.Fatal("expected error for missing dictionary")
	}
}

func TestSpecFileBytes(t *testing.T) {
	raw, err := SpecFileBytes("FIX44.xml")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 100000 {
		t.Fatalf("FIX44.xml suspiciously small: %d bytes", len(raw))
	}
}

const testCustomDictXML = `<?xml version="1.0" encoding="UTF-8"?>
<fix major="4" minor="4" servicepack="0" type="FIX">
  <header/>
  <trailer/>
  <fields>
    <field number="8" name="BeginString" type="STRING"/>
    <field number="35" name="MsgType" type="STRING">
      <value enum="D" description="NewOrderSingle"/>
    </field>
    <field number="9001" name="MyCustomField" type="STRING">
      <value enum="A" description="Alpha"/>
      <value enum="B" description="Beta"/>
    </field>
  </fields>
  <messages>
    <message name="NewOrderSingle" msgtype="D" msgcat="app"/>
  </messages>
</fix>`

func TestParseBytes_CustomDictionary(t *testing.T) {
	d, err := ParseBytes([]byte(testCustomDictXML))
	if err != nil {
		t.Fatal(err)
	}
	if d.BeginString != "FIX.4.4" {
		t.Fatalf("BeginString = %q, want FIX.4.4", d.BeginString)
	}
	f, ok := d.Field(9001)
	if !ok {
		t.Fatal("tag 9001 not found")
	}
	if f.Name != "MyCustomField" {
		t.Fatalf("tag 9001 name = %q, want MyCustomField", f.Name)
	}
	if got := d.EnumDescription(9001, "A"); got != "Alpha" {
		t.Fatalf("9001=A enum = %q, want Alpha", got)
	}
	if got := d.EnumDescription(9001, "B"); got != "Beta" {
		t.Fatalf("9001=B enum = %q, want Beta", got)
	}
	if d.MessageName("D") != "NewOrderSingle" {
		t.Fatalf("msgType D name = %q", d.MessageName("D"))
	}
	if len(d.Fields) != 3 {
		t.Fatalf("fields = %d, want 3", len(d.Fields))
	}
}

func TestParseBytes_FIXTType(t *testing.T) {
	xml := `<fix major="1" minor="1" type="FIXT"><fields><field number="8" name="BeginString" type="STRING"/></fields></fix>`
	d, err := ParseBytes([]byte(xml))
	if err != nil {
		t.Fatal(err)
	}
	if d.BeginString != "FIXT.1.1" {
		t.Fatalf("BeginString = %q, want FIXT.1.1", d.BeginString)
	}
}

func TestParseBytes_ValidationFailures(t *testing.T) {
	cases := []struct {
		name string
		xml  string
		want string // substring of the error
	}{
		{"malformed", `<fix major="4" minor="4"><fields><field`, "malformed XML"},
		{"wrong root", `<config><fields><field number="1" name="X" type="STRING"/></fields></config>`, "not a FIX data dictionary"},
		{"no version", `<fix><fields><field number="1" name="X" type="STRING"/></fields></fix>`, "major/minor"},
		{"no fields", `<fix major="4" minor="4"><fields></fields></fix>`, "no <field> definitions"},
		{"bad number", `<fix major="4" minor="4"><fields><field number="abc" name="X" type="STRING"/></fields></fix>`, "invalid number"},
		{"no name", `<fix major="4" minor="4"><fields><field number="9001" type="STRING"/></fields></fix>`, "has no name"},
		{"duplicate", `<fix major="4" minor="4"><fields><field number="9001" name="A" type="STRING"/><field number="9001" name="B" type="STRING"/></fields></fix>`, "duplicate definition"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseBytes([]byte(c.xml))
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), c.want)
			}
		})
	}
}

func TestLoad_StillParsesEmbedded(t *testing.T) {
	// ParseBytes is now the single parse path: every embedded spec must
	// still load through it.
	for _, f := range []string{"FIX40.xml", "FIX42.xml", "FIX44.xml", "FIXT11.xml", "FIX50SP2.xml"} {
		if _, err := Load(f); err != nil {
			t.Fatalf("Load(%s): %v", f, err)
		}
	}
}
