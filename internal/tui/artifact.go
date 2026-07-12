package tui

import "fmt"

func scrollIndicator(offset, visible, total int) string {
	if total <= visible || total <= 0 {
		return ""
	}
	offset = clampInt(offset, 0, maxInt(0, total-visible))
	end := minInt(total, offset+visible)
	percent := int(float64(end) / float64(total) * 100)
	return subtleStyle.Render(fmt.Sprintf("↕ 第 %d-%d/%d 行 · %d%%", offset+1, end, total, percent))
}
