package session

import (
	"sync"
	"testing"

	"fixlab.dev/fixlab/backend/internal/dictionary"
)

const testCustomDict = `<fix major="4" minor="4" type="FIX">
<fields>
<field number="9001" name="MyCustomField" type="STRING">
<value enum="A" description="Alpha"/>
<value enum="B" description="Beta"/>
</field>
</fields>
<messages>
<message name="NewOrderSingle" msgtype="D" msgcat="app"/>
</messages>
</fix>`

func testDictSession(t *testing.T) *Session {
	t.Helper()
	base, err := dictionary.Load("FIX44.xml")
	if err != nil {
		t.Fatal(err)
	}
	return &Session{baseDict: base}
}

func TestDictionary_DefaultIsBase(t *testing.T) {
	s := testDictSession(t)
	if s.Dictionary() != s.baseDict {
		t.Fatal("Dictionary() should return the base dictionary when no override is set")
	}
	if s.Dictionary().FieldName(9001) != "" {
		t.Fatal("base dictionary should not know tag 9001")
	}
	if _, _, ok := s.CustomDictionaryInfo(); ok {
		t.Fatal("CustomDictionaryInfo should report no override")
	}
	info := s.DictionaryInfo()
	if info.Custom {
		t.Fatal("DictionaryInfo.Custom should be false")
	}
	if info.BeginString != "FIX.4.4" {
		t.Fatalf("BeginString = %q", info.BeginString)
	}
}

func TestDictionary_SetAndClear(t *testing.T) {
	s := testDictSession(t)
	custom, err := dictionary.ParseBytes([]byte(testCustomDict))
	if err != nil {
		t.Fatal(err)
	}
	s.SetCustomDictionary("acme", custom)

	if got := s.Dictionary().FieldName(9001); got != "MyCustomField" {
		t.Fatalf("tag 9001 name = %q, want MyCustomField", got)
	}
	if got := s.Dictionary().EnumDescription(9001, "B"); got != "Beta" {
		t.Fatalf("9001=B enum = %q, want Beta", got)
	}
	name, _, ok := s.CustomDictionaryInfo()
	if !ok || name != "acme" {
		t.Fatalf("CustomDictionaryInfo = %q, %v", name, ok)
	}
	info := s.DictionaryInfo()
	if !info.Custom || info.Name != "acme" || info.Fields != 1 || info.Messages != 1 {
		t.Fatalf("DictionaryInfo = %+v", info)
	}

	s.ClearCustomDictionary()
	if s.Dictionary() != s.baseDict {
		t.Fatal("Dictionary() should return the base dictionary after clear")
	}
	if _, _, ok := s.CustomDictionaryInfo(); ok {
		t.Fatal("override should be gone after clear")
	}
}

func TestDictionary_ConcurrentSwap(t *testing.T) {
	s := testDictSession(t)
	custom, err := dictionary.ParseBytes([]byte(testCustomDict))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if i%2 == 0 {
					s.SetCustomDictionary("x", custom)
				} else {
					s.ClearCustomDictionary()
				}
				_ = s.Dictionary().FieldName(11) // must never race/panic
				_ = s.DictionaryInfo()
			}
		}(i)
	}
	wg.Wait()
}
