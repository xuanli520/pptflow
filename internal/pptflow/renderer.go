package pptflow

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"fmt"
	"html"
	"os"
	"strings"
)

func RenderPPTX(graph ObjectGraph) ([]byte, error) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	media := mediaAssets(graph)
	if err := writePPTXPart(writer, "[Content_Types].xml", contentTypes(len(graph.Slides), chartCount(graph))); err != nil {
		return nil, err
	}
	if err := writePPTXPart(writer, "_rels/.rels", packageRels()); err != nil {
		return nil, err
	}
	if err := writePPTXPart(writer, "docProps/core.xml", coreProps()); err != nil {
		return nil, err
	}
	if err := writePPTXPart(writer, "docProps/app.xml", appProps(len(graph.Slides))); err != nil {
		return nil, err
	}
	if err := writePPTXPart(writer, "ppt/presentation.xml", presentationXML(len(graph.Slides), graph.Template)); err != nil {
		return nil, err
	}
	if err := writePPTXPart(writer, "ppt/_rels/presentation.xml.rels", presentationRels(len(graph.Slides))); err != nil {
		return nil, err
	}
	if err := writePPTXPart(writer, "ppt/presProps.xml", presPropsXML()); err != nil {
		return nil, err
	}
	if err := writePPTXPart(writer, "ppt/viewProps.xml", viewPropsXML()); err != nil {
		return nil, err
	}
	if err := writePPTXPart(writer, "ppt/tableStyles.xml", tableStylesXML()); err != nil {
		return nil, err
	}
	if err := writePPTXPart(writer, "ppt/theme/theme1.xml", themeXML(graph.Template)); err != nil {
		return nil, err
	}
	if err := writePPTXPart(writer, "ppt/slideMasters/slideMaster1.xml", slideMasterXML()); err != nil {
		return nil, err
	}
	if err := writePPTXPart(writer, "ppt/slideMasters/_rels/slideMaster1.xml.rels", slideMasterRels()); err != nil {
		return nil, err
	}
	if err := writePPTXPart(writer, "ppt/slideLayouts/slideLayout1.xml", slideLayoutXML()); err != nil {
		return nil, err
	}
	if err := writePPTXPart(writer, "ppt/slideLayouts/_rels/slideLayout1.xml.rels", slideLayoutRels()); err != nil {
		return nil, err
	}
	chartIndex := 1
	for i, slide := range graph.Slides {
		relationships := slideRelationships(slide, chartIndex)
		if err := writePPTXPart(writer, fmt.Sprintf("ppt/slides/slide%d.xml", i+1), slideXML(slide, &chartIndex)); err != nil {
			return nil, err
		}
		if err := writePPTXPart(writer, fmt.Sprintf("ppt/slides/_rels/slide%d.xml.rels", i+1), relationships); err != nil {
			return nil, err
		}
	}
	for i := 1; i < chartIndex; i++ {
		if err := writePPTXPart(writer, fmt.Sprintf("ppt/charts/chart%d.xml", i), chartXML()); err != nil {
			return nil, err
		}
	}
	for name, data := range media {
		part, err := writer.Create("ppt/media/" + name)
		if err != nil {
			return nil, err
		}
		if _, err := part.Write(data); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func writePPTXPart(writer *zip.Writer, name, content string) error {
	part, err := writer.Create(name)
	if err != nil {
		return err
	}
	_, err = part.Write([]byte(content))
	return err
}

func contentTypes(slides, charts int) string {
	var builder strings.Builder
	builder.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	builder.WriteString(`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">`)
	builder.WriteString(`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>`)
	builder.WriteString(`<Default Extension="xml" ContentType="application/xml"/>`)
	builder.WriteString(`<Default Extension="png" ContentType="image/png"/>`)
	builder.WriteString(`<Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/>`)
	builder.WriteString(`<Override PartName="/docProps/app.xml" ContentType="application/vnd.openxmlformats-officedocument.extended-properties+xml"/>`)
	builder.WriteString(`<Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/>`)
	builder.WriteString(`<Override PartName="/ppt/presProps.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presProps+xml"/>`)
	builder.WriteString(`<Override PartName="/ppt/viewProps.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.viewProps+xml"/>`)
	builder.WriteString(`<Override PartName="/ppt/tableStyles.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.tableStyles+xml"/>`)
	builder.WriteString(`<Override PartName="/ppt/theme/theme1.xml" ContentType="application/vnd.openxmlformats-officedocument.theme+xml"/>`)
	builder.WriteString(`<Override PartName="/ppt/slideMasters/slideMaster1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slideMaster+xml"/>`)
	builder.WriteString(`<Override PartName="/ppt/slideLayouts/slideLayout1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slideLayout+xml"/>`)
	for i := 1; i <= slides; i++ {
		builder.WriteString(fmt.Sprintf(`<Override PartName="/ppt/slides/slide%d.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slide+xml"/>`, i))
	}
	for i := 1; i <= charts; i++ {
		builder.WriteString(fmt.Sprintf(`<Override PartName="/ppt/charts/chart%d.xml" ContentType="application/vnd.openxmlformats-officedocument.drawingml.chart+xml"/>`, i))
	}
	builder.WriteString(`</Types>`)
	return builder.String()
}

func packageRels() string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="ppt/presentation.xml"/><Relationship Id="rId2" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/core-properties" Target="docProps/core.xml"/><Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/extended-properties" Target="docProps/app.xml"/></Relationships>`
}

func coreProps() string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:dcterms="http://purl.org/dc/terms/"><dc:title>PPTflow Phase 0 Deck</dc:title><dc:creator>PPTflow</dc:creator></cp:coreProperties>`
}

func appProps(slides int) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Properties xmlns="http://schemas.openxmlformats.org/officeDocument/2006/extended-properties"><Application>PPTflow</Application><Slides>%d</Slides></Properties>`, slides)
}

func presentationXML(slides int, template TemplateProfile) string {
	width := template.SlideWidth
	height := template.SlideHeight
	if width == 0 {
		width = 12192000
	}
	if height == 0 {
		height = 6858000
	}
	var builder strings.Builder
	builder.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	builder.WriteString(`<p:presentation xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" saveSubsetFonts="1" autoCompressPictures="0">`)
	builder.WriteString(`<p:sldMasterIdLst><p:sldMasterId id="2147483648" r:id="rId1"/></p:sldMasterIdLst>`)
	builder.WriteString(`<p:sldIdLst>`)
	for i := 1; i <= slides; i++ {
		builder.WriteString(fmt.Sprintf(`<p:sldId id="%d" r:id="rId%d"/>`, 255+i, i+5))
	}
	builder.WriteString(`</p:sldIdLst>`)
	builder.WriteString(fmt.Sprintf(`<p:sldSz cx="%d" cy="%d" type="wide"/><p:notesSz cx="6858000" cy="9144000"/>`, width, height))
	builder.WriteString(defaultTextStyleXML())
	builder.WriteString(`</p:presentation>`)
	return builder.String()
}

func presentationRels(slides int) string {
	var builder strings.Builder
	builder.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	builder.WriteString(`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideMaster" Target="slideMasters/slideMaster1.xml"/>`)
	builder.WriteString(`<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/presProps" Target="presProps.xml"/>`)
	builder.WriteString(`<Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/viewProps" Target="viewProps.xml"/>`)
	builder.WriteString(`<Relationship Id="rId4" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/theme" Target="theme/theme1.xml"/>`)
	builder.WriteString(`<Relationship Id="rId5" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/tableStyles" Target="tableStyles.xml"/>`)
	for i := 1; i <= slides; i++ {
		builder.WriteString(fmt.Sprintf(`<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide" Target="slides/slide%d.xml"/>`, i+5, i))
	}
	builder.WriteString(`</Relationships>`)
	return builder.String()
}

func themeXML(template TemplateProfile) string {
	accent := template.ThemeColors["accent"]
	if accent == "" {
		accent = "2A9D8F"
	}
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><a:theme xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" name="PPTflow"><a:themeElements><a:clrScheme name="PPTflow"><a:dk1><a:srgbClr val="111111"/></a:dk1><a:lt1><a:srgbClr val="FFFFFF"/></a:lt1><a:dk2><a:srgbClr val="14213D"/></a:dk2><a:lt2><a:srgbClr val="F8F9FA"/></a:lt2><a:accent1><a:srgbClr val="` + accent + `"/></a:accent1><a:accent2><a:srgbClr val="E76F51"/></a:accent2><a:accent3><a:srgbClr val="F4A261"/></a:accent3><a:accent4><a:srgbClr val="264653"/></a:accent4><a:accent5><a:srgbClr val="457B9D"/></a:accent5><a:accent6><a:srgbClr val="8D99AE"/></a:accent6><a:hlink><a:srgbClr val="0563C1"/></a:hlink><a:folHlink><a:srgbClr val="954F72"/></a:folHlink></a:clrScheme><a:fontScheme name="PPTflow"><a:majorFont><a:latin typeface="Aptos"/><a:ea typeface="Microsoft YaHei"/><a:cs typeface="Arial"/></a:majorFont><a:minorFont><a:latin typeface="Aptos"/><a:ea typeface="Microsoft YaHei"/><a:cs typeface="Arial"/></a:minorFont></a:fontScheme><a:fmtScheme name="PPTflow"><a:fillStyleLst><a:solidFill><a:schemeClr val="phClr"/></a:solidFill><a:gradFill rotWithShape="1"><a:gsLst><a:gs pos="0"><a:schemeClr val="phClr"><a:tint val="50000"/><a:satMod val="300000"/></a:schemeClr></a:gs><a:gs pos="100000"><a:schemeClr val="phClr"><a:tint val="15000"/><a:satMod val="350000"/></a:schemeClr></a:gs></a:gsLst><a:lin ang="16200000" scaled="1"/></a:gradFill><a:gradFill rotWithShape="1"><a:gsLst><a:gs pos="0"><a:schemeClr val="phClr"><a:tint val="100000"/><a:shade val="100000"/></a:schemeClr></a:gs><a:gs pos="100000"><a:schemeClr val="phClr"><a:tint val="50000"/><a:shade val="100000"/></a:schemeClr></a:gs></a:gsLst><a:lin ang="16200000" scaled="0"/></a:gradFill></a:fillStyleLst><a:lnStyleLst><a:ln w="9525" cap="flat" cmpd="sng" algn="ctr"><a:solidFill><a:schemeClr val="phClr"/></a:solidFill><a:prstDash val="solid"/></a:ln><a:ln w="25400" cap="flat" cmpd="sng" algn="ctr"><a:solidFill><a:schemeClr val="phClr"/></a:solidFill><a:prstDash val="solid"/></a:ln><a:ln w="38100" cap="flat" cmpd="sng" algn="ctr"><a:solidFill><a:schemeClr val="phClr"/></a:solidFill><a:prstDash val="solid"/></a:ln></a:lnStyleLst><a:effectStyleLst><a:effectStyle><a:effectLst/></a:effectStyle><a:effectStyle><a:effectLst><a:outerShdw blurRad="40000" dist="20000" dir="5400000" rotWithShape="0"><a:srgbClr val="000000"><a:alpha val="35000"/></a:srgbClr></a:outerShdw></a:effectLst></a:effectStyle><a:effectStyle><a:effectLst><a:outerShdw blurRad="40000" dist="23000" dir="5400000" rotWithShape="0"><a:srgbClr val="000000"><a:alpha val="35000"/></a:srgbClr></a:outerShdw></a:effectLst></a:effectStyle></a:effectStyleLst><a:bgFillStyleLst><a:solidFill><a:schemeClr val="phClr"/></a:solidFill><a:gradFill rotWithShape="1"><a:gsLst><a:gs pos="0"><a:schemeClr val="phClr"><a:tint val="40000"/><a:satMod val="350000"/></a:schemeClr></a:gs><a:gs pos="100000"><a:schemeClr val="phClr"><a:shade val="20000"/><a:satMod val="255000"/></a:schemeClr></a:gs></a:gsLst><a:path path="circle"><a:fillToRect l="50000" t="-80000" r="50000" b="180000"/></a:path></a:gradFill><a:gradFill rotWithShape="1"><a:gsLst><a:gs pos="0"><a:schemeClr val="phClr"><a:tint val="80000"/><a:satMod val="300000"/></a:schemeClr></a:gs><a:gs pos="100000"><a:schemeClr val="phClr"><a:shade val="30000"/><a:satMod val="200000"/></a:schemeClr></a:gs></a:gsLst><a:path path="circle"><a:fillToRect l="50000" t="50000" r="50000" b="50000"/></a:path></a:gradFill></a:bgFillStyleLst></a:fmtScheme></a:themeElements><a:objectDefaults/><a:extraClrSchemeLst/></a:theme>`
}

func presPropsXML() string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><p:presentationPr xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"/>`
}

func viewPropsXML() string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><p:viewPr xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" lastView="sldThumbnailView"><p:normalViewPr><p:restoredLeft sz="15620"/><p:restoredTop sz="94660"/></p:normalViewPr><p:slideViewPr><p:cSldViewPr snapToGrid="0" snapToObjects="1"><p:cViewPr varScale="1"><p:scale><a:sx n="100" d="100"/><a:sy n="100" d="100"/></p:scale><p:origin x="0" y="0"/></p:cViewPr></p:cSldViewPr></p:slideViewPr><p:notesTextViewPr><p:cViewPr><p:scale><a:sx n="100" d="100"/><a:sy n="100" d="100"/></p:scale><p:origin x="0" y="0"/></p:cViewPr></p:notesTextViewPr><p:gridSpacing cx="76200" cy="76200"/></p:viewPr>`
}

func tableStylesXML() string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><a:tblStyleLst xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" def="{5C22544A-7EE6-4342-B048-85BDC9FD1C3A}"/>`
}

func slideMasterXML() string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><p:sldMaster xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"><p:cSld><p:bg><p:bgRef idx="1001"><a:schemeClr val="bg1"/></p:bgRef></p:bg><p:spTree><p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="0" cy="0"/><a:chOff x="0" y="0"/><a:chExt cx="0" cy="0"/></a:xfrm></p:grpSpPr></p:spTree></p:cSld><p:clrMap bg1="lt1" tx1="dk1" bg2="lt2" tx2="dk2" accent1="accent1" accent2="accent2" accent3="accent3" accent4="accent4" accent5="accent5" accent6="accent6" hlink="hlink" folHlink="folHlink"/><p:sldLayoutIdLst><p:sldLayoutId id="2147483649" r:id="rId1"/></p:sldLayoutIdLst><p:txStyles><p:titleStyle><a:lvl1pPr algn="ctr"><a:defRPr sz="4400"><a:solidFill><a:schemeClr val="tx1"/></a:solidFill><a:latin typeface="+mj-lt"/><a:ea typeface="+mj-ea"/><a:cs typeface="+mj-cs"/></a:defRPr></a:lvl1pPr></p:titleStyle><p:bodyStyle><a:lvl1pPr marL="342900" indent="-342900"><a:defRPr sz="2800"><a:solidFill><a:schemeClr val="tx1"/></a:solidFill><a:latin typeface="+mn-lt"/><a:ea typeface="+mn-ea"/><a:cs typeface="+mn-cs"/></a:defRPr></a:lvl1pPr></p:bodyStyle><p:otherStyle><a:defPPr><a:defRPr lang="zh-CN"/></a:defPPr><a:lvl1pPr><a:defRPr sz="1800"><a:solidFill><a:schemeClr val="tx1"/></a:solidFill><a:latin typeface="+mn-lt"/><a:ea typeface="+mn-ea"/><a:cs typeface="+mn-cs"/></a:defRPr></a:lvl1pPr></p:otherStyle></p:txStyles></p:sldMaster>`
}

func slideMasterRels() string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideLayout" Target="../slideLayouts/slideLayout1.xml"/><Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/theme" Target="../theme/theme1.xml"/></Relationships>`
}

func slideLayoutXML() string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><p:sldLayout xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" type="blank" preserve="1"><p:cSld name="Blank"><p:spTree><p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="0" cy="0"/><a:chOff x="0" y="0"/><a:chExt cx="0" cy="0"/></a:xfrm></p:grpSpPr></p:spTree></p:cSld><p:clrMapOvr><a:masterClrMapping/></p:clrMapOvr></p:sldLayout>`
}

func slideLayoutRels() string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideMaster" Target="../slideMasters/slideMaster1.xml"/></Relationships>`
}

func defaultTextStyleXML() string {
	return `<p:defaultTextStyle><a:defPPr><a:defRPr lang="zh-CN"/></a:defPPr><a:lvl1pPr marL="0" algn="l" defTabSz="457200"><a:defRPr sz="1800" kern="1200"><a:solidFill><a:schemeClr val="tx1"/></a:solidFill><a:latin typeface="+mn-lt"/><a:ea typeface="+mn-ea"/><a:cs typeface="+mn-cs"/></a:defRPr></a:lvl1pPr></p:defaultTextStyle>`
}

func slideXML(slide Slide, chartIndex *int) string {
	var builder strings.Builder
	builder.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	builder.WriteString(`<p:sld xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:c="http://schemas.openxmlformats.org/drawingml/2006/chart"><p:cSld><p:spTree>`)
	builder.WriteString(`<p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="0" cy="0"/><a:chOff x="0" y="0"/><a:chExt cx="0" cy="0"/></a:xfrm></p:grpSpPr>`)
	objectID := 2
	relID := 2
	for _, object := range slide.Objects {
		switch object.Type {
		case "text_box":
			builder.WriteString(shapeXML(objectID, object, true))
		case "shape":
			builder.WriteString(shapeXML(objectID, object, false))
		case "connector":
			builder.WriteString(connectorXML(objectID, object))
		case "picture":
			builder.WriteString(pictureXML(objectID, object, relID))
			relID++
		case "table":
			builder.WriteString(tableXML(objectID, object))
		case "chart":
			builder.WriteString(chartFrameXML(objectID, object, relID))
			relID++
			*chartIndex = *chartIndex + 1
		}
		objectID++
	}
	builder.WriteString(`</p:spTree></p:cSld><p:clrMapOvr><a:masterClrMapping/></p:clrMapOvr></p:sld>`)
	return builder.String()
}

func slideRelationships(slide Slide, chartStart int) string {
	var builder strings.Builder
	builder.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	builder.WriteString(`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideLayout" Target="../slideLayouts/slideLayout1.xml"/>`)
	rid := 2
	chartID := chartStart
	for _, object := range slide.Objects {
		switch object.Type {
		case "picture":
			builder.WriteString(fmt.Sprintf(`<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="../media/%s.png"/>`, rid, mediaID(object.Image)))
			rid++
		case "chart":
			builder.WriteString(fmt.Sprintf(`<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/chart" Target="../charts/chart%d.xml"/>`, rid, chartID))
			rid++
			chartID++
		}
	}
	builder.WriteString(`</Relationships>`)
	return builder.String()
}

func shapeXML(id int, object PPTObject, textBox bool) string {
	prst := object.Shape
	if prst == "" {
		prst = "rect"
	}
	fill := "FFFFFF"
	if object.Style != nil && object.Style["fill"] != "" {
		fill = object.Style["fill"]
	}
	txBox := ""
	if textBox {
		txBox = ` txBox="1"`
	}
	return fmt.Sprintf(`<p:sp><p:nvSpPr><p:cNvPr id="%d" name="%s"/><p:cNvSpPr%s/><p:nvPr/></p:nvSpPr><p:spPr>%s<a:prstGeom prst="%s"><a:avLst/></a:prstGeom><a:solidFill><a:srgbClr val="%s"/></a:solidFill><a:ln><a:solidFill><a:srgbClr val="2A9D8F"/></a:solidFill></a:ln></p:spPr><p:txBody><a:bodyPr wrap="square"/><a:lstStyle/><a:p><a:r><a:rPr lang="zh-CN" sz="2200"/><a:t>%s</a:t></a:r></a:p></p:txBody></p:sp>`, id, xmlText(object.ID), txBox, xfrm(object), prst, fill, xmlText(object.Text))
}

func connectorXML(id int, object PPTObject) string {
	return fmt.Sprintf(`<p:cxnSp><p:nvCxnSpPr><p:cNvPr id="%d" name="%s"/><p:cNvCxnSpPr/><p:nvPr/></p:nvCxnSpPr><p:spPr>%s<a:prstGeom prst="line"><a:avLst/></a:prstGeom><a:ln w="25400"><a:solidFill><a:srgbClr val="2A9D8F"/></a:solidFill><a:tailEnd type="triangle"/></a:ln></p:spPr></p:cxnSp>`, id, xmlText(object.ID), xfrm(object))
}

func pictureXML(id int, object PPTObject, relID int) string {
	return fmt.Sprintf(`<p:pic><p:nvPicPr><p:cNvPr id="%d" name="%s"/><p:cNvPicPr/><p:nvPr/></p:nvPicPr><p:blipFill><a:blip r:embed="rId%d"/><a:stretch><a:fillRect/></a:stretch></p:blipFill><p:spPr>%s<a:prstGeom prst="rect"><a:avLst/></a:prstGeom></p:spPr></p:pic>`, id, xmlText(object.ID), relID, xfrm(object))
}

func tableXML(id int, object PPTObject) string {
	rows := object.Rows
	if len(rows) == 0 {
		rows = [][]string{{"A", "B"}, {"1", "2"}}
	}
	cols := len(rows[0])
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf(`<p:graphicFrame><p:nvGraphicFramePr><p:cNvPr id="%d" name="%s"/><p:cNvGraphicFramePr/><p:nvPr/></p:nvGraphicFramePr>%s<a:graphic><a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/table"><a:tbl><a:tblPr firstRow="1"><a:tableStyleId>{5C22544A-7EE6-4342-B048-85BDC9FD1C3A}</a:tableStyleId></a:tblPr><a:tblGrid>`, id, xmlText(object.ID), xfrm(object)))
	for i := 0; i < cols; i++ {
		builder.WriteString(`<a:gridCol w="2600000"/>`)
	}
	builder.WriteString(`</a:tblGrid>`)
	for _, row := range rows {
		builder.WriteString(`<a:tr h="520000">`)
		for i := 0; i < cols; i++ {
			text := ""
			if i < len(row) {
				text = row[i]
			}
			builder.WriteString(`<a:tc><a:txBody><a:bodyPr/><a:lstStyle/><a:p><a:r><a:rPr lang="zh-CN" sz="1600"/><a:t>`)
			builder.WriteString(xmlText(text))
			builder.WriteString(`</a:t></a:r></a:p></a:txBody><a:tcPr/></a:tc>`)
		}
		builder.WriteString(`</a:tr>`)
	}
	builder.WriteString(`</a:tbl></a:graphicData></a:graphic></p:graphicFrame>`)
	return builder.String()
}

func chartFrameXML(id int, object PPTObject, relID int) string {
	return fmt.Sprintf(`<p:graphicFrame><p:nvGraphicFramePr><p:cNvPr id="%d" name="%s"/><p:cNvGraphicFramePr/><p:nvPr/></p:nvGraphicFramePr>%s<a:graphic><a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/chart"><c:chart r:id="rId%d"/></a:graphicData></a:graphic></p:graphicFrame>`, id, xmlText(object.ID), xfrm(object), relID)
}

func chartXML() string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><c:chartSpace xmlns:c="http://schemas.openxmlformats.org/drawingml/2006/chart" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><c:date1904 val="0"/><c:lang val="zh-CN"/><c:roundedCorners val="0"/><c:chart><c:autoTitleDeleted val="0"/><c:plotArea><c:layout/><c:barChart><c:barDir val="col"/><c:grouping val="clustered"/><c:varyColors val="0"/><c:ser><c:idx val="0"/><c:order val="0"/><c:tx><c:strRef><c:f>Sheet1!$B$1</c:f><c:strCache><c:ptCount val="1"/><c:pt idx="0"><c:v>Growth</c:v></c:pt></c:strCache></c:strRef></c:tx><c:cat><c:strLit><c:ptCount val="4"/><c:pt idx="0"><c:v>Q1</c:v></c:pt><c:pt idx="1"><c:v>Q2</c:v></c:pt><c:pt idx="2"><c:v>Q3</c:v></c:pt><c:pt idx="3"><c:v>Q4</c:v></c:pt></c:strLit></c:cat><c:val><c:numLit><c:formatCode>General</c:formatCode><c:ptCount val="4"/><c:pt idx="0"><c:v>24</c:v></c:pt><c:pt idx="1"><c:v>38</c:v></c:pt><c:pt idx="2"><c:v>57</c:v></c:pt><c:pt idx="3"><c:v>81</c:v></c:pt></c:numLit></c:val></c:ser><c:dLbls><c:showLegendKey val="0"/><c:showVal val="0"/><c:showCatName val="0"/><c:showSerName val="0"/><c:showPercent val="0"/><c:showBubbleSize val="0"/></c:dLbls><c:axId val="12345678"/><c:axId val="12345679"/></c:barChart><c:catAx><c:axId val="12345678"/><c:scaling><c:orientation val="minMax"/></c:scaling><c:delete val="0"/><c:axPos val="b"/><c:majorTickMark val="none"/><c:minorTickMark val="none"/><c:tickLblPos val="nextTo"/><c:crossAx val="12345679"/><c:crosses val="autoZero"/><c:auto val="1"/><c:lblAlgn val="ctr"/><c:lblOffset val="100"/><c:noMultiLvlLbl val="0"/></c:catAx><c:valAx><c:axId val="12345679"/><c:scaling><c:orientation val="minMax"/></c:scaling><c:delete val="0"/><c:axPos val="l"/><c:numFmt formatCode="General" sourceLinked="1"/><c:majorTickMark val="none"/><c:minorTickMark val="none"/><c:tickLblPos val="nextTo"/><c:crossAx val="12345678"/><c:crosses val="autoZero"/><c:crossBetween val="between"/></c:valAx></c:plotArea><c:legend><c:legendPos val="r"/><c:layout/><c:overlay val="0"/></c:legend><c:plotVisOnly val="1"/><c:dispBlanksAs val="gap"/><c:showDLblsOverMax val="0"/></c:chart><c:printSettings><c:headerFooter/><c:pageMargins l="0.7" r="0.7" t="0.75" b="0.75" header="0.3" footer="0.3"/><c:pageSetup/></c:printSettings></c:chartSpace>`
}

func xfrm(object PPTObject) string {
	return fmt.Sprintf(`<a:xfrm><a:off x="%d" y="%d"/><a:ext cx="%d" cy="%d"/></a:xfrm>`, emu(object.X), emu(object.Y), emu(object.W), emu(object.H))
}

func emu(inches float64) int64 {
	return int64(inches * 914400)
}

func xmlText(value string) string {
	return html.EscapeString(value)
}

func chartCount(graph ObjectGraph) int {
	count := 0
	for _, slide := range graph.Slides {
		for _, object := range slide.Objects {
			if object.Type == "chart" {
				count++
			}
		}
	}
	return count
}

func mediaAssets(graph ObjectGraph) map[string][]byte {
	result := map[string][]byte{}
	placeholder, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAFgwJ/lr9mogAAAABJRU5ErkJggg==")
	result["placeholder.png"] = placeholder
	for _, asset := range graph.Assets.Images {
		data, err := os.ReadFile(asset.Path)
		if err != nil || len(data) == 0 {
			continue
		}
		result[mediaID(asset.ID)+".png"] = data
	}
	return result
}

func mediaID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "placeholder"
	}
	var builder strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			builder.WriteRune(r)
		}
	}
	if builder.Len() == 0 {
		return "placeholder"
	}
	return builder.String()
}
