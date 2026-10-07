// Package specs embeds the standard FIX data dictionaries shipped with
// the backend (spec §17). The canonical files live here at
// backend/specs/ per the repository layout in spec §46; they are compiled
// into the binary so the server has no runtime file dependency on them.
package specs

import "embed"

//go:embed *.xml
var files embed.FS

// ReadFile returns the raw bytes of an embedded dictionary file,
// e.g. "FIX44.xml".
func ReadFile(name string) ([]byte, error) {
	return files.ReadFile(name)
}

// Names lists the embedded dictionary file names.
func Names() []string {
	entries, err := files.ReadDir(".")
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
