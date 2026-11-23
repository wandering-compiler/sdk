package i18n

import (
	"fmt"

	"github.com/leonelquinteros/gotext"
)

// CatalogStats counts what a `.po` catalog says about its translation.
type CatalogStats struct {
	// Total is the active entries (the harvested msgids).
	Total int
	// Translated are served: a msgstr, not fuzzy.
	Translated int
	// Untranslated have an empty msgstr; the runtime falls back.
	Untranslated int
	// Fuzzy carry a guess a translator has not reviewed (a changed source
	// sentence, MergePO); not served until the flag is removed.
	Fuzzy int
	// Obsolete are `#~` entries kept for their translation; never served.
	Obsolete int
	// Problems is ValidatePO's answer apart from the fuzzy notices.
	Problems []error
}

// Stats reads body. In the default language an entry whose msgstr is its
// msgid counts as translated: the baseline is the translation.
func Stats(body []byte) CatalogStats {
	var s CatalogStats
	po := gotext.NewPo()
	po.Parse(body)
	fuzzy := fuzzyIDs(body)
	if d := po.GetDomain(); d != nil {
		for _, t := range d.GetTranslations() {
			if t == nil || t.ID == "" {
				continue
			}
			s.Total++
			switch {
			case fuzzy[t.ID]:
				s.Fuzzy++
			case rawMsgstr(t) == "":
				s.Untranslated++
			default:
				s.Translated++
			}
		}
	}
	s.Obsolete = len(parseObsolete(body))
	errs, entries := scanPO(body)
	s.Problems = errs
	for _, e := range entries {
		switch {
		case e.ctxt != "":
			s.Problems = append(s.Problems, fmt.Errorf("line %d: entry %q has msgctxt %q; message contexts are not supported and the entry is not served", e.line, e.id, e.ctxt))
		case e.plural:
			s.Problems = append(s.Problems, fmt.Errorf("line %d: entry %q has plural forms; plurals are not supported and the entry is not served", e.line, e.id))
		}
	}
	return s
}
