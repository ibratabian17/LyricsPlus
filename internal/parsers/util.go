package parsers

import (
	"sort"
	"strconv"
)

func sortStrings(s []string) {
	sort.Strings(s)
}

func partNameForI(i int) string {
	names := []string{"Instrumental", "Intro", "Verse", "Pre-Chorus", "Chorus", "Bridge", "Outro"}
	if i-1 < len(names) {
		return names[i-1]
	}
	return "Part " + strconv.Itoa(i)
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
