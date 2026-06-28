package pptflow

type Requirements struct {
	Scenario   string   `json:"scenario"`
	Topic      string   `json:"topic"`
	Audience   string   `json:"audience"`
	Tone       string   `json:"tone"`
	SlideCount int      `json:"slide_count"`
	MustHave   []string `json:"must_have"`
}

type TemplateProfile struct {
	Name         string            `json:"name"`
	SlideWidth   int64             `json:"slide_width"`
	SlideHeight  int64             `json:"slide_height"`
	ThemeColors  map[string]string `json:"theme_colors"`
	Fonts        []string          `json:"fonts"`
	Layouts      []string          `json:"layouts"`
	Placeholders []string          `json:"placeholders"`
}

type ContentPlan struct {
	Scenario  string           `json:"scenario"`
	Narrative string           `json:"narrative"`
	Sections  []ContentSection `json:"sections"`
}

type ContentSection struct {
	Title  string   `json:"title"`
	Points []string `json:"points"`
}

type SlidePlan struct {
	Slides []SlideIntent `json:"slides"`
}

type SlideIntent struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Layout  string   `json:"layout"`
	Objects []string `json:"objects"`
}

type ObjectGraph struct {
	SchemaVersion string          `json:"schema_version"`
	Template      TemplateProfile `json:"template"`
	Assets        AssetsManifest  `json:"assets,omitempty"`
	Slides        []Slide         `json:"slides"`
}

type Slide struct {
	ID      string      `json:"id"`
	Title   string      `json:"title"`
	Layout  string      `json:"layout"`
	Objects []PPTObject `json:"objects"`
}

type PPTObject struct {
	ID       string            `json:"id"`
	Type     string            `json:"type"`
	Text     string            `json:"text,omitempty"`
	X        float64           `json:"x"`
	Y        float64           `json:"y"`
	W        float64           `json:"w"`
	H        float64           `json:"h"`
	Style    map[string]string `json:"style,omitempty"`
	Rows     [][]string        `json:"rows,omitempty"`
	Series   []ChartSeries     `json:"series,omitempty"`
	Shape    string            `json:"shape,omitempty"`
	Image    string            `json:"image,omitempty"`
	Children []PPTObject       `json:"children,omitempty"`
}

type ChartSeries struct {
	Name   string    `json:"name"`
	Labels []string  `json:"labels"`
	Values []float64 `json:"values"`
}

type SchemaReport struct {
	OK         bool     `json:"ok"`
	Errors     []string `json:"errors,omitempty"`
	SlideCount int      `json:"slide_count"`
}

type EditabilityReport struct {
	OK         bool           `json:"ok"`
	Counts     map[string]int `json:"counts"`
	SlideCount int            `json:"slide_count"`
	Errors     []string       `json:"errors,omitempty"`
}

type VisualReport struct {
	OK       bool     `json:"ok"`
	Errors   []string `json:"errors,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

type RepairPlan struct {
	Required bool     `json:"required"`
	Actions  []string `json:"actions"`
}

type AssetsManifest struct {
	Images []ImageAsset `json:"images"`
}

type ImageAsset struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Prompt string `json:"prompt"`
	Model  string `json:"model"`
	Size   string `json:"size"`
	Source string `json:"source"`
}
