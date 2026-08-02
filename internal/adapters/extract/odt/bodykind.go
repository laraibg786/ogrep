package odt

import (
	"archive/zip"
	"encoding/xml"
	"io"
)

// bodyKind streams content.xml only as far as needed to find
// office:body's first child element and returns its local name
// ("text", "spreadsheet", "presentation", ...), the ODF-defined
// discriminator between document kinds that all otherwise share the
// exact same content.xml part name. Used only as a fallback when the
// package's mimetype part is missing or unreadable; see Sniff's doc
// comment for why the mimetype part is checked first.
func bodyKind(f *zip.File) (string, error) {
	rc, err := f.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()

	dec := xml.NewDecoder(rc)
	inBody := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if se.Name.Local == "body" {
			inBody = true
			continue
		}
		if inBody {
			return se.Name.Local, nil
		}
	}
}
