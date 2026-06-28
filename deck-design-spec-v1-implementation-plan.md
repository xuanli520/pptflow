# DeckDesignSpec v1 Implementation Plan

## 背景

当前 PPTflow Phase 0 已能生成可打开的 PPTX，但生成质量仍停留在“对象拼装稿”：固定坐标、固定文本框、固定图片位、固定表格和图表。问题不在 PPTX zip 或 OpenXML 是否成立，而在缺少“设计中间层”。

DeckDesignSpec v1 的目标是在 `ContentPlan` 和 `ObjectGraph` 之间增加一个独立插件模块，把“怎么讲、怎么排、怎么生成图片、怎么设计表格图表、怎么控制动效”结构化下来。

目标链路：

```text
Requirements
  -> ContentPlan
  -> DeckDesignSpec
  -> ObjectGraph
  -> PPTX
```

非目标：

- 不在本计划中编码。
- 不保留旧 QA 流程。
- 不把 QA 作为 PPTflow 内部模块。
- 不继续用旧 p2r_tui / prompt2repo / pipeline 概念。
- 不把 `DeckDesignSpec` 做成 renderer 私有结构，它必须是独立插件产物。

## 调研结论

### 商用品质 PPT 的关键设计规则

1. **每页必须有结论型标题**
   Slideworks 对咨询式 action title 的总结是：标题应表达该页的核心结论，而不是只写主题词。think-cell 对 Pyramid Principle 的说明也支持“先结论、后证据”的表达方式。

2. **需要稳定模板、母版和版式系统**
   Johns Hopkins 品牌指南强调：品牌模板的作用是让字体、颜色、背景、位置、图片使用方式保持一致；图片应增加上下文，而不是装饰拼贴。

3. **可读性必须进入设计约束**
   Microsoft PowerPoint accessibility 指南要求：每页有唯一标题，使用足够对比度，字体不小于 18pt，使用无衬线字体，内容有足够留白，表格尽量简单。

4. **视觉层级要可计算**
   NN/g 将视觉层级归纳为颜色和对比、尺度、分组和留白。对 PPTflow 来说，这意味着每页要明确 `primary_focus`，并限制大字号对象、强调色对象和信息密度。

5. **图表和表格要传达信息，而不是只画数据**
   Harvard data visualization accessibility 指南强调：图表应简单、熟悉、不过载；颜色不能是唯一含义载体；重要元素需要标签、图例或直接标注；文本对比度至少 4.5:1，图形对象对比度目标 3:1。

6. **动效应该是语义化、节制的**
   Microsoft Morph 可以表达对象位置、尺度和状态变化；WebAIM 和 Harvard 都提醒动画会干扰理解，尤其复杂或无控制的动画。商用 PPT 中动效应限于 fade、step reveal、Morph emphasis 这类低噪声模式。

## 当前代码事实

- `internal/workflow/types.go` 已定义 `Plugin`、`PluginManifest`、`NodeSpec`、`NodeResult`、`ArtifactStore`。
- `internal/workflow/registry.go` 已支持按插件 ID 和 node kind 注册、查找。
- `internal/pptflow/workflow.go` 当前 Phase 0 workflow 仍包含 `editability_verify`、`visual_verify`、`repair_plan`。
- `internal/pptflow/types.go` 当前只有 `Requirements`、`TemplateProfile`、`ContentPlan`、`SlidePlan`、`ObjectGraph`、`AssetsManifest` 等底层结构。
- `internal/pptflow/plugin.go` 当前一个插件承载 requirements、template、content、slide plan、asset、object graph、render、verification、package 等多类职责。

## 架构决策

### Decision

新增独立插件包 `internal/designspec`，插件 ID 为 `pptflow.designspec`，输出 `deck_design_spec.json`。主 workflow 使用该插件替代当前粗糙 `slide_plan -> object_graph` 的直接跳转。

### Drivers

- 商用品质来自设计系统和叙事结构，不来自 renderer 层硬编码。
- 设计中间层必须可测试、可版本化、可单独演进。
- QA 属于另一个项目，本项目只保留 schema contract 和 deterministic validation，不保留 QA 流程命名和节点。
- Go 插件边界应保持小接口和清晰行为，符合 Go 官方对 interface 的使用建议。

### Alternatives Considered

1. **继续扩展 `ObjectGraph`**
   放弃。`ObjectGraph` 是渲染对象层，不适合承载叙事、版式意图、图片 brief、动效策略。

2. **把设计逻辑写进 renderer**
   放弃。renderer 应只消费已确定的对象图和主题，不应决定设计。

3. **把 `DeckDesignSpec` 做成 `internal/pptflow` 内部函数**
   放弃。用户要求该模块也作为插件存在；独立插件也更利于后续替换为 agent/LLM 设计器。

4. **保留 visual/editability verification**
   放弃。用户明确要求 QA 完全从本项目删除。后续只允许保留 contract validation，不使用 QA 命名和流程。

## 插件边界

### 新插件

Package:

```text
internal/designspec
```

Plugin manifest:

```text
ID: pptflow.designspec
Version: 0.1.0
Kinds:
  - pptflow.designspec.plan
  - pptflow.designspec.validate
```

后续可扩展但 v1 暂不增加：

```text
pptflow.designspec.agent_refine
pptflow.designspec.template_fit
pptflow.designspec.style_pack_load
```

### 现有插件调整

`internal/pptflow` 保留核心生产节点：

```text
pptflow.requirements_fixture
pptflow.template_introspect
pptflow.content_plan
pptflow.asset_prepare
pptflow.object_graph_build
pptflow.schema_verify
pptflow.pptx_render
pptflow.package
```

删除或停止注册：

```text
pptflow.slide_plan
pptflow.editability_verify
pptflow.visual_verify
pptflow.repair_plan
```

`slide_plan` 的职责被 `deck_design_spec` 覆盖。`editability_verify`、`visual_verify`、`repair_plan` 不再属于本项目。

## DeckDesignSpec v1 数据模型

顶层结构：

```json
{
  "schema_version": "pptflow.deck_design_spec.v1",
  "deck": {},
  "theme": {},
  "narrative": {},
  "slides": [],
  "asset_plan": {},
  "constraints": {}
}
```

### deck

```json
{
  "scenario": "roadshow",
  "topic": "AI hardware product launch",
  "audience": "channel partners and media",
  "tone": ["premium", "concise", "product-led"],
  "slide_count": 10,
  "aspect_ratio": "16:9",
  "locale": "zh-CN"
}
```

### theme

```json
{
  "style_pack": "premium_business_light",
  "canvas": {
    "background": "light_canvas",
    "safe_margin_in": 0.55,
    "grid_columns": 12,
    "grid_rows": 8
  },
  "colors": {
    "background": "#FFFFFF",
    "foreground": "#14213D",
    "muted": "#E9ECEF",
    "accent": "#2A9D8F",
    "warning": "#E76F51"
  },
  "typography": {
    "latin": "Aptos",
    "cjk": "Microsoft YaHei",
    "deck_title_pt": 52,
    "slide_title_pt": 36,
    "section_header_pt": 24,
    "body_pt": 18,
    "caption_pt": 12
  },
  "motion": {
    "default_transition": "fade",
    "allowed": ["none", "fade", "step_reveal", "morph_emphasis"],
    "max_animated_groups_per_slide": 3
  }
}
```

### narrative

```json
{
  "story_arc": "problem_solution_evidence_action",
  "thesis": "AI hardware product launch improves meeting productivity from capture to decisions",
  "title_chain": [
    "AI 硬件新品把会议效率从记录提升到决策",
    "会议协作的主要损耗发生在会后整理和跟进",
    "产品能力将记录、提炼和任务分发合为一个闭环"
  ]
}
```

### slide

```json
{
  "id": "slide-03",
  "role": "solution",
  "action_title": "产品能力将记录、提炼和任务分发合为一个闭环",
  "layout_archetype": "three_pillar_with_visual",
  "density": "medium",
  "primary_focus": "capability_pillars",
  "content_blocks": [],
  "visual_blocks": [],
  "chart_blocks": [],
  "table_blocks": [],
  "motion_plan": {}
}
```

### content_blocks

```json
{
  "id": "slide-03-copy-01",
  "kind": "body",
  "text": "自动记录、重点提炼、任务分发",
  "importance": "primary",
  "max_lines": 2,
  "font_role": "section_header"
}
```

### visual_blocks

```json
{
  "id": "slide-03-visual-01",
  "kind": "image2",
  "purpose": "explain product workflow in a premium business visual",
  "composition": "right-side product workflow scene, left side clean whitespace",
  "style": "premium business product illustration",
  "aspect_ratio": "16:9",
  "crop": "cover",
  "no_text": true,
  "reuse_policy": "unique"
}
```

### chart_blocks

```json
{
  "id": "slide-05-chart-01",
  "chart_type": "bar",
  "message": "Q4 efficiency reaches target range",
  "highlight": {
    "series": "efficiency",
    "point": "Q4"
  },
  "labeling": "direct",
  "gridlines": "minimal",
  "legend": "none_if_direct_labels"
}
```

### table_blocks

```json
{
  "id": "slide-06-table-01",
  "purpose": "compare launch channels",
  "structure": {
    "header": true,
    "max_rows": 5,
    "max_columns": 4,
    "no_merged_cells": true
  },
  "emphasis": {
    "highlight_rows": ["recommended"],
    "numeric_alignment": "right"
  }
}
```

### constraints

```json
{
  "min_body_font_pt": 16,
  "min_title_font_pt": 35,
  "text_contrast_ratio_min": 4.5,
  "object_contrast_ratio_min": 3.0,
  "max_primary_focus_count": 1,
  "max_big_elements": 2,
  "max_body_lines_per_slide": 8,
  "forbid_text_in_generated_images": true,
  "forbid_overlaps": true,
  "forbid_old_qa_nodes": true
}
```

## Workflow v1

目标 workflow：

```text
requirements
  -> template
  -> content_plan
  -> designspec_plan
  -> designspec_validate
  -> asset_prepare
  -> object_graph
  -> schema_verify
  -> pptx_render
  -> package
```

节点说明：

- `designspec_plan`：读取 `requirements.json`、`template_profile.json`、`content_plan.json`，输出 `deck_design_spec.json`。
- `designspec_validate`：读取 `deck_design_spec.json`，输出 `deck_design_spec_report.json`。
- `asset_prepare`：改为读取 `DeckDesignSpec.visual_blocks`，为 image2 生成更具体的 asset brief。
- `object_graph`：改为读取 `DeckDesignSpec`，不再从 `SlidePlan` 直接生成固定坐标对象。
- `schema_verify`：只验证对象图 contract，不做 QA。

## 文件规划

新增：

```text
internal/designspec/plugin.go
internal/designspec/types.go
internal/designspec/planner.go
internal/designspec/validate.go
internal/designspec/register.go
```

调整：

```text
internal/pptflow/workflow.go
internal/pptflow/plugin.go
internal/pptflow/types.go
internal/pptflow/renderer.go
internal/app/phase0.go
cmd/phase0.go
```

删除或迁移出本项目：

```text
editability_verify node
visual_verify node
repair_plan node
SlidePlan if fully superseded
```

## 实施步骤

### Phase 1: Contract first

1. 定义 `DeckDesignSpec` Go struct。
2. 所有 struct 字段带 JSON tag。
3. 顶层必须包含 `SchemaVersion string`。
4. 增加 schema version 常量：`pptflow.deck_design_spec.v1`。
5. 增加 deterministic validator，只校验结构和设计约束，不做 QA。

验收：

- 能序列化稳定 JSON。
- 缺少 `schema_version`、`slides`、`theme`、`action_title` 时失败。
- 所有 slide 有唯一 ID 和唯一 action title。

### Phase 2: Plugin split

1. 新增 `internal/designspec` 插件。
2. 在 app 初始化时注册 `pptflow.designspec`。
3. `designspec_plan` 生成 `deck_design_spec.json`。
4. `designspec_validate` 生成 `deck_design_spec_report.json`。
5. 从 `internal/pptflow` 停止注册旧 QA-adjacent 节点。

验收：

- `workflow.Registry.Manifests()` 能看到 `pptflow.designspec`。
- workflow 不再出现 `editability_verify`、`visual_verify`、`repair_plan`。
- repo 内不出现 `internal/qa`、`qa workflow`、旧 p2r/prompt2repo 标识。

### Phase 3: Layout archetype compiler

1. 建立 `layout_archetype -> object graph` 编译器。
2. v1 支持 8 个 archetype：
   - `cover_hero`
   - `hero_image_split`
   - `three_pillar`
   - `big_number_evidence`
   - `chart_with_callout`
   - `comparison_matrix`
   - `roadmap_timeline`
   - `executive_summary`
3. 每个 archetype 固定网格、边距、信息密度和 object role。
4. 禁止直接随机坐标。

验收：

- 同一 spec 多次编译输出稳定。
- 所有元素落在 safe margin 内。
- 每页只有一个 primary focus。

### Phase 4: Image2 asset brief upgrade

1. `asset_prepare` 改为读取 `visual_blocks`。
2. image2 prompt 从通用商务插图升级为结构化 brief。
3. prompt 明确主体位置、留白方向、用途、裁切策略、禁止文字。
4. `AssetsManifest` 记录 `brief_id`、`slide_id`、`purpose`、`composition`。

验收：

- 每张生成图都有对应 `visual_block.id`。
- 图片 prompt 不再只是 `clean premium business illustration`。
- 同一图片不得无理由复用。

### Phase 5: Table and chart design model

1. 表格支持 header、斑马纹、重点行、数字右对齐、无合并单元格。
2. 图表支持 message、highlight、direct labels、callout、minimal gridlines。
3. renderer 只消费 chart/table design tokens。

验收：

- 表格不超过 v1 限制行列。
- chart 有明确 message 和 highlight。
- 图表颜色不只靠颜色表达含义。

### Phase 6: Motion semantics

1. Spec 中只声明 motion intent。
2. v1 支持：
   - `none`
   - `fade`
   - `step_reveal`
   - `morph_emphasis`
3. 默认关闭复杂动画。
4. 每页 animated groups 不超过 3。

验收：

- 无 motion plan 时生成静态 PPT。
- 有 motion plan 时仍能在 PowerPoint/WPS 打开。
- 动效不影响内容阅读顺序。

### Phase 7: Local validation, not QA

保留 deterministic validation，删除 QA 概念。

允许：

```text
schema validation
contract validation
openxml structural validation
render smoke check
```

禁止：

```text
qa
QA
quality assurance pipeline
visual QA
repair QA
old tui QA
```

验收：

- workflow 节点名不含 `qa`。
- package 名不含 `qa`。
- artifact 名不含 `qa_report`。
- CLI/TUI 文案不出现 QA 主流程。

## 验收标准

1. `DeckDesignSpec` 作为独立插件产物存在。
2. 主流程不再依赖 `SlidePlan` 的固定对象清单。
3. 生成的 `deck_design_spec.json` 能解释每页为什么这样设计。
4. `ObjectGraph` 只做渲染对象层，不承载叙事决策。
5. image2 资产由 `visual_blocks` 驱动。
6. 表格和图表由设计模型驱动，而不是 renderer 临时决定样式。
7. 项目中没有旧 QA 主流程、旧 QA TUI、旧 p2r/prompt2repo 概念。
8. 新 workflow 输出 PPTX 可打开，并包含 master/layout/theme/tableStyles。
9. 没有 API key 落盘。

## 风险

### 风险 1: DeckDesignSpec 过大

缓解：v1 只做 8 个 archetype，不做完整设计语言。

### 风险 2: 插件边界过碎

缓解：v1 只拆出 `pptflow.designspec`，不把 chart/table/image 都拆成独立插件。

### 风险 3: validation 被重新做成 QA

缓解：命名上严格使用 `validate`、`contract`、`structural`，不使用 QA；职责只做确定性 contract 检查。

### 风险 4: OpenXML renderer 继续膨胀

缓解：renderer 只处理已编译 ObjectGraph，不承担设计决策。后续如需要，可把 renderer 抽成 `pptflow.openxml` 插件，但不在 v1 做。

### 风险 5: 中文内容继续乱码

缓解：DeckDesignSpec 阶段统一要求 UTF-8 输入、输出和 artifact 写入；所有 fixture、默认文案、JSON marshal 都必须是 UTF-8。

## 参考资料

- Microsoft PowerPoint accessibility: https://support.microsoft.com/en-US/accessibility/powerpoint/make-your-powerpoint-presentations-accessible-to-people-with-disabilities
- Microsoft Morph transition: https://support.microsoft.com/en-US/PowerPoint/morph-transition-tips-and-tricks
- WebAIM PowerPoint accessibility: https://webaim.org/techniques/powerpoint/
- Johns Hopkins branded PowerPoint template guidance: https://brand.jhu.edu/blog/building-effective-presentations-with-a-branded-powerpoint-template/
- Nielsen Norman Group visual hierarchy: https://www.nngroup.com/articles/visual-hierarchy-ux-definition/
- Harvard data visualization accessibility: https://accessibility.huit.harvard.edu/data-viz-charts-graphs
- Slideworks action titles: https://slideworks.io/resources/how-to-write-action-titles-like-mckinsey
- think-cell Pyramid Principle: https://www.think-cell.com/en/resources/content-hub/using-the-pyramid-principle-to-build-better-powerpoint-presentations
- JSON Schema docs: https://json-schema.org/learn/getting-started-step-by-step
- Couchbase schema versioning: https://developer.couchbase.com/tutorial-schema-versioning/
- Effective Go interfaces: https://go.dev/doc/effective_go
- Go struct tags: https://go.dev/wiki/Well-known-struct-tags
