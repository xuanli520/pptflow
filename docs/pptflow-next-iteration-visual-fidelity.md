# PPTflow Next Iteration: Editable PPT Visual Fidelity

## 背景

当前目标产物：

`manual_eval/e2e_editable_fixed_20260706-173919_artifacts/promptflow-v2-20260706T093922Z-921621200/deck.pptx`

这个 deck 已经能成功生成，也没有退回整页图片。布局位置基本跟随原型图，但格式、样式和图片背景明显粗糙。

本轮结论：问题主因不是 Image2 原图质量，而是中间 `layout_analysis` 和本地 OOXML renderer 丢失视觉语义。原型图在 `slide_images/slide_01.png`、`slide_images/slide_02.png` 中视觉完整；生成预览 `preview/slide_01.png`、`preview/slide_02.png` 出现圆形变方块、状态胶囊文字缺失或样式丢失、字体层级丢失、时间轴线条和节点退化、图标白底残留。

## 证据

- `layout_analysis.json` 当前共 65 个 region：`shape=37`、`body_text=20`、`image=4`、`decoration=2`、`title=2`。
- 37 个 `shape` 都没有 `shape_kind/fill/stroke/radius/shadow/text` 等可渲染样式字段。
- `extracted/*.png` 四个图片资源采样结果均无透明像素，说明当前资源提取保留了白底。
- `deck.pptx` 的 slide XML 已有 `<p:sp>` 和 `<p:pic>`，说明可编辑路线成立，但对象质量不足。
- `validatePPTX` 只检查 zip/XML/页数，不检查视觉质量、关键文本、对象类型或透明背景。

## 根因

1. `LayoutRegion` contract 太薄。

   `internal/promptflow/types.go` 的 `LayoutRegion` 只包含 bbox、z-order、基础文本字段、图片描述、chart/table hint。它不能表达圆角卡片、椭圆节点、线条、虚线、描边、阴影、透明度、富文本 runs、shape 内文字、父子分组。

2. `buildLayoutAnalysisPrompt` 没要求模型输出可渲染样式。

   `internal/promptflow/prompts.go` 只要求 `title/body_text/image/chart/table/shape/decoration`，其中 `shape` 是裸类型，没有强制输出 `shape/fill/stroke/shadow/text`。结果是 renderer 只能猜。

3. renderer 只能生成文本框、矩形和图片。

   `internal/promptflow/plugin.go` 中：

   - `pptxRectShape` 固定 `prst="rect"`，无描边、无圆角、无椭圆、无线条、无阴影。
   - `shapeFillColor` 依赖 ID 启发式，只覆盖 `bar/dot/divider/axis/bracket`，大量元素走默认浅灰。
   - `pptxTextShape` 和 `pptxParagraphs` 只有单字号、单颜色，无 bold、runs、行距、垂直对齐、边距。
   - `pptxRectShape` 不支持 shape 内文字，状态 pill、编号圆点、badge 文字会丢。
   - `pptxPicture` 使用 stretch fillRect，资源没有透明背景策略。

4. `extractResources` 不是 Image2 资源重建，而是本地裁剪。

   `internal/promptflow/plugin.go` 的 `extractResources` 当前调用 `cropRegionFromSlide`，输出 `local-crop`。这能保证稳定，但会把图标周围白底一起裁出。

5. 工作流缺少视觉质量门禁和局部 repair。

   `internal/promptflow/workflow.go` 当前是 8 个串行大节点，`analyze_layout` 单 turn 分析所有 slide。生成 PPT 后没有 `render_preview`、`visual_qa`、`repair_layout` 或 `repair_pptx` 节点。

## 方向

下一阶段从“整页图生成后视觉反推 PPT”调整为“结构化设计 IR 优先，图片只作为资产和视觉参考”。

短期仍保留当前链路，但把 `layout_analysis` 升级为 renderer contract，先把可编辑 PPT 的视觉保真拉上来。不要改回整页图片方案。

## 迭代 1: Layout Analysis Schema v2

目标文件：

- `internal/promptflow/types.go`
- `internal/promptflow/prompts.go`
- `internal/promptflow/parse.go`
- `internal/promptflow/plugin_test.go`

新增 schema：

- `schema_version`: `pptflow.layout_analysis.v2`
- `slides[].elements` 替代或兼容 `slides[].regions`
- `type`: `text | shape | line | image | icon | chart | table | group`
- `role`: `title | subtitle | body | metric | metadata | card | divider | status_pill | badge | timeline_axis | timeline_tick | timeline_node | connector | icon_bg | icon_glyph`
- `group_id`
- `parent_id`
- `opacity`
- `shape.kind`: `rect | round_rect | ellipse | triangle | freeform`
- `shape.corner_radius`
- `fill.type`: `none | solid | gradient`
- `fill.color`
- `fill.alpha`
- `stroke.type`: `none | solid`
- `stroke.color`
- `stroke.alpha`
- `stroke.width_pt`
- `stroke.dash`: `solid | dash | dot`
- `stroke.cap`
- `shadow.enabled`
- `shadow.color`
- `shadow.alpha`
- `shadow.blur_pt`
- `shadow.distance_pt`
- `shadow.angle`
- `text.content`
- `text.role`
- `text.font_family`
- `text.font_size_pt`
- `text.font_weight`
- `text.color`
- `text.alignment`
- `text.vertical_alignment`
- `text.margin_left_pt`
- `text.margin_right_pt`
- `text.margin_top_pt`
- `text.margin_bottom_pt`
- `text.runs[]`
- `image.asset_kind`
- `image.background_strategy`
- `image.has_alpha`
- `image.crop_hint`
- `image.fit`: `contain | cover | stretch`
- `image.mask_shape`

Go 类型建议：

- `LayoutElement`
- `ShapeSpec`
- `FillSpec`
- `StrokeSpec`
- `ShadowSpec`
- `TextSpec`
- `TextRunSpec`
- `ImageSpec`

Prompt 改造要求：

- 禁止裸 `shape`。每个 shape 必须有 `shape/fill/stroke/shadow`。
- 所有可见文字必须转录，包括 status pill、badge、圆点数字。
- 细线必须输出 `type: "line"`，不能用高 0.01 的矩形伪装。
- 空心节点必须表达为 `fill:white + stroke`，实心节点表达为蓝色 fill。
- 圆角卡片必须输出 `round_rect + corner_radius + shadow`。
- 同一文本框内有层级时使用 `runs`，例如 `Owner:` 和 `Due:` bold，值 regular。
- 透明图标输出 `asset_kind: "transparent_icon"` 和 `background_strategy: "remove"`。
- 时间轴必须使用统一 `group_id`，axis、tick、node、connector、milestone label 归组。

验收：

- 当前两页样例中 `shape_with_style_fields / shape_count = 100%`。
- `status_pill` 有 `round_rect`、蓝色 fill、白色 text。
- timeline axis/tick/node/connector 全部是 `line` 或 `ellipse`，不再是浅灰矩形。
- title、metric、body、metadata 至少 4 个文字层级可由字段区分。

## 迭代 2: OOXML Renderer v2

目标文件：

- `internal/promptflow/plugin.go`
- 后续拆分为 `renderer.go`、`pptx_ooxml.go`
- `internal/promptflow/plugin_test.go`

目标函数：

- `pptxEditableSlide`
- `pptxTextShape`
- `pptxParagraphs`
- `pptxPicture`
- `pptxRectShape`
- 新增 `pptxShape`
- 新增 `pptxLineShape`
- 新增 `pptxEllipseShape`
- 新增 `pptxRoundRectShape`
- 新增 `pptxShapeTextBody`
- 新增 `pptxFill`
- 新增 `pptxStroke`
- 新增 `pptxShadow`

必须支持：

- `rect`
- `roundRect`
- `ellipse`
- `line`
- `dash`
- `stroke`
- `fill`
- `opacity`
- `shadow`
- shape 内文本
- rich text runs
- paragraph spacing
- vertical anchor
- text margins
- image contain/cover/crop

短期兜底：

在 schema v2 全面稳定前，可以用 ID/role 兜底修复当前样例：

- `status` -> roundRect + blue fill + white text
- `number/badge/node` -> ellipse
- `tick/connector/timeline_line/rule/divider` -> line + stroke
- `snapshot_card` -> roundRect + white fill + subtle shadow

但兜底只能作为过渡，最终以 schema v2 字段为准。

测试：

- `TestPPTXShapeRendererWritesEllipseRoundRectLine`
- `TestPPTXShapeRendererWritesFillStrokeDashShadow`
- `TestPPTXShapeRendererWritesShapeText`
- `TestPPTXTextShapeWritesRunsBoldColorAndSpacing`
- `TestPPTXPictureUsesFitModeWithoutDistortion`

Golden XML fixture：

从当前 artifact 抽最小 fixture：

- `layout_analysis.json`
- `style_spec.json`
- `extracted_resource_manifest.json`

断言：

- status pill 是 `roundRect` 且 XML 中有 `At Risk`、`In Progress`。
- priority number、milestone badge、timeline node 是 `ellipse`。
- 蓝色元素包含 `2563EB` 或 style token accent。
- timeline 线条不是浅灰矩形。
- `Owner:`、`Due:` 使用 bold run。

## 迭代 3: Transparent Resource Extraction

目标文件：

- `internal/promptflow/plugin.go`
- 后续拆分为 `resources.go`
- `internal/runtime/image2/runtime.go`

目标：

- 本地 crop 保留为 fallback。
- 对 `image.asset_kind = transparent_icon | logo | illustration` 的资源，优先走 Image2 source-image extraction。
- 输出透明 PNG，保留 alpha。
- `ExtractedResource.Properties` 写入：
  - `transparent`
  - `background_removed`
  - `extraction_method`
  - `source_region_id`
  - `fit`
  - `mask_shape`

当前不能改的点：

- `OPENAI_API_KEY` fallback 到 `https://new-api.metalics.cn/v1` 是已确认正确行为，不作为本轮问题处理。

验收：

- 当前四个 `extracted/*.png` 至少图标外圈之外区域存在 alpha。
- `preview/slide_01.png` 中 rocket icon 不再出现白色方块。
- `pptxPicture` 对透明 PNG 不添加白色底。

## 迭代 4: Render Preview + Visual QA + Repair

目标文件：

- `internal/promptflow/workflow.go`
- `internal/promptflow/plugin.go`
- `internal/workflow/types.go`
- `internal/workflow/engine.go`

新增节点：

1. `render_pptx_preview`
2. `visual_qa`
3. `repair_plan`
4. `repair_apply`

`visual_qa_report.json` 至少包含：

- slide count
- render size
- editable text shape count
- shape count
- picture count
- full-slide image detection
- key text coverage
- shape style coverage
- transparent resource coverage
- overlap/clipping warnings
- color token hit rate
- bbox drift
- optional SSIM/pHash/pixel diff

阻断规则：

- 禁止 full-slide image fallback 被当成成功。
- 可见文本 95% 以上必须是 PPT text。
- 几何元素 90% 以上必须是 PPT shape/line。
- `shape` 缺 `shape/fill/stroke` 时 QA 失败。
- 透明资源缺 alpha 时 QA 失败。
- 关键文本缺失时 QA 失败。
- preview 与 target image 差异超过阈值时 QA 失败。

Repair 策略：

- 优先只修失败 slide。
- 优先修 `layout_analysis` 或 renderer 参数，不重跑整套图片生成。
- 对 schema 缺字段的失败，回到 `analyze_layout` 的单页 repair。
- 对 OOXML 缺能力的失败，进入 renderer backlog。

## 迭代 5: Workflow 和配置整理

目标文件：

- `internal/promptflow/workflow.go`
- `internal/app/promptflow.go`
- `cmd/promptflow.go`
- `internal/runtime/image2/runtime.go`
- `internal/workflow/types.go`

目标：

- 引入 `RunProfile`：
  - `quality_mode`
  - `providers`
  - `timeouts`
  - `retry_policy`
  - `fallback_policy`
  - `qa_thresholds`
- provider 显式配置：
  - agent provider
  - image provider
  - base_url
  - api_key_env
  - model
  - capabilities
  - retry/backoff
- fallback 枚举：
  - `strict`
  - `dev_placeholder`
  - `image_backed`
  - `partial_editable`
- 生产默认 `strict`。
- placeholder 只允许开发模式。
- timeout 按阶段配置，不再用一个 `codexTimeout` 覆盖所有 Agent 节点。
- 图片节点 timeout 按 slide/asset 数计算。
- runtime 只报告 provider 错误和能力，不自行决定 placeholder。

注意：

- 不要把 `OPENAI_API_KEY` fallback 到第三方 base URL 当作 bug 修掉。
- 需要改的是 provider profile 显式化和 fallback 决策位置。

## 代码质量目标

`internal/promptflow/plugin.go` 已经超过 1200 行，职责混合：

- agent prompt
- parse fallback
- image generation
- resource extraction
- PPTX package writing
- OOXML fragments
- validation

下一阶段拆分：

- `analysis.go`
- `resources.go`
- `renderer.go`
- `pptx_ooxml.go`
- `visual_qa.go`
- `fallback.go`

拆分原则：

- 先搬运，不改行为。
- 再按迭代 1 到 4 增量改 contract 和 renderer。
- 测试先覆盖 public behavior 和关键 XML，不追求每个 helper 都加测试。

## 验收标准

以当前 artifact 作为第一组回归样例：

`manual_eval/e2e_editable_fixed_20260706-173919_artifacts/promptflow-v2-20260706T093922Z-921621200`

必须满足：

- `deck.pptx` 可打开、可编辑、可保存重开。
- 不存在整页图片 fallback。
- `preview/slide_01.png` 中圆角卡片、阴影、蓝色状态 pill、编号圆点、图标透明背景可见。
- `preview/slide_02.png` 中 timeline axis、tick、node、connector、badge 形状正确。
- 所有 status pill、编号、badge 文字在 XML 中可编辑。
- `Owner:`、`Due:` 有独立 bold run。
- bbox 误差 <= 0.10 inch。
- 颜色 RGB 单通道误差 <= 20。
- 字体大小误差 <= 2 pt。
- 线宽误差 <= 1 pt。
- visible text editability >= 95%。
- geometric editability >= 90%。
- transparent icon resources coverage = 100%。

## 推荐实施顺序

1. 给 `LayoutRegion` 加 v2 样式字段，并更新 `buildLayoutAnalysisPrompt`。
2. 扩展 renderer 支持 shape kind、fill、stroke、line、ellipse、roundRect、shape text。
3. 增加 rich text runs。
4. 修透明资源提取和 `pptxPicture` fit 策略。
5. 新增 render preview 和 visual QA。
6. 拆分 `plugin.go`。
7. 引入 `RunProfile` 和 fallback policy。

## 非目标

- 不回退到整页图片 PPT。
- 不把所有图表和表格长期当截图。
- 不修改已确认的 Image2 provider fallback 行为。
- 不为每个 helper 机械新增测试。
