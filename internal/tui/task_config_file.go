package tui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

const (
	taskInputConfigFormat   = "harbor.task-input.v1"
	taskInputConfigVersion  = "1"
	taskInputConfigMaxBytes = 1 << 20
)

// taskInputConfig is the file-backed equivalent of the editable task form.
// It intentionally contains no lifecycle or execution state.
type taskInputConfig struct {
	Format        string `json:"format"`
	Version       string `json:"version"`
	RepositoryURL string `json:"repository_url"`
	CommitSHA     string `json:"commit_sha"`
	BaseImage     string `json:"base_image"`
	Slug          string `json:"slug"`
	Title         string `json:"title"`
	TaskType      string `json:"task_type"`
	Application   string `json:"application"`
	CodeLanguage  string `json:"code_language"`
	Is0To1        bool   `json:"is_0_to_1"`
	Objective     string `json:"objective"`
	Reason        string `json:"reason"`
}

func readTaskInputConfigFile(path string) (taskInputConfig, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return taskInputConfig{}, errors.New("配置文件路径不能为空")
	}
	entry, err := os.Lstat(path)
	if err != nil {
		return taskInputConfig{}, fmt.Errorf("读取配置文件: %w", err)
	}
	if entry.Mode()&os.ModeSymlink != 0 || !entry.Mode().IsRegular() {
		return taskInputConfig{}, errors.New("配置文件必须是普通文件")
	}
	if entry.Size() > taskInputConfigMaxBytes {
		return taskInputConfig{}, fmt.Errorf("配置文件不能超过 %d 字节", taskInputConfigMaxBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return taskInputConfig{}, fmt.Errorf("打开配置文件: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(entry, opened) {
		return taskInputConfig{}, errors.New("配置文件在读取期间发生变化")
	}
	raw, err := io.ReadAll(io.LimitReader(file, taskInputConfigMaxBytes+1))
	if err != nil {
		return taskInputConfig{}, fmt.Errorf("读取配置文件: %w", err)
	}
	if len(raw) > taskInputConfigMaxBytes {
		return taskInputConfig{}, fmt.Errorf("配置文件不能超过 %d 字节", taskInputConfigMaxBytes)
	}
	if !utf8.Valid(raw) || bytes.IndexByte(raw, 0) >= 0 {
		return taskInputConfig{}, errors.New("配置文件必须是有效 UTF-8 JSON")
	}
	if err := rejectDuplicateTaskConfigJSONKeys(raw); err != nil {
		return taskInputConfig{}, fmt.Errorf("校验配置 JSON: %w", err)
	}
	var config taskInputConfig
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return taskInputConfig{}, fmt.Errorf("解析配置 JSON: %w", err)
	}
	if err := ensureSingleTaskConfigJSONDocument(decoder); err != nil {
		return taskInputConfig{}, err
	}
	if err := config.validate(); err != nil {
		return taskInputConfig{}, err
	}
	return config, nil
}

func (config *taskInputConfig) validate() error {
	if config.Format != taskInputConfigFormat || config.Version != taskInputConfigVersion {
		return fmt.Errorf("配置格式必须为 %q，版本必须为 %q", taskInputConfigFormat, taskInputConfigVersion)
	}
	fields := []struct {
		name  string
		value *string
		limit int
	}{
		{"repository_url", &config.RepositoryURL, 256},
		{"commit_sha", &config.CommitSHA, 64},
		{"base_image", &config.BaseImage, 512},
		{"slug", &config.Slug, 80},
		{"title", &config.Title, 160},
		{"task_type", &config.TaskType, 64},
		{"application", &config.Application, 64},
		{"code_language", &config.CodeLanguage, 64},
		{"objective", &config.Objective, 512},
		{"reason", &config.Reason, 240},
	}
	for _, field := range fields {
		*field.value = strings.TrimSpace(*field.value)
		if *field.value == "" {
			return fmt.Errorf("配置字段 %s 不能为空", field.name)
		}
		if utf8.RuneCountInString(*field.value) > field.limit {
			return fmt.Errorf("配置字段 %s 不能超过 %d 个字符", field.name, field.limit)
		}
	}
	if err := validateTaskInputContractTokens(config.Slug, config.CodeLanguage, config.TaskType, config.Application); err != nil {
		return err
	}
	if len(config.CommitSHA) != 40 && len(config.CommitSHA) != 64 {
		return errors.New("配置字段 commit_sha 必须是 40 或 64 位提交哈希")
	}
	for _, character := range config.CommitSHA {
		if !(character >= '0' && character <= '9') && !(character >= 'a' && character <= 'f') {
			return errors.New("配置字段 commit_sha 必须是小写十六进制提交哈希")
		}
	}
	return nil
}

func ensureSingleTaskConfigJSONDocument(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); errors.Is(err, io.EOF) {
		return nil
	} else if err == nil {
		return errors.New("配置文件只能包含一个 JSON 文档")
	} else {
		return fmt.Errorf("解析配置 JSON: %w", err)
	}
}

func rejectDuplicateTaskConfigJSONKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := walkTaskConfigJSONValue(decoder, "$"); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("配置文件只能包含一个 JSON 文档")
		}
		return err
	}
	return nil
}

func walkTaskConfigJSONValue(decoder *json.Decoder, location string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("%s 的对象键不是字符串", location)
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("%s 存在重复键 %q", location, key)
			}
			seen[key] = struct{}{}
			if err := walkTaskConfigJSONValue(decoder, location+"."+key); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("JSON 对象未正确结束")
		}
	case '[':
		for index := 0; decoder.More(); index++ {
			if err := walkTaskConfigJSONValue(decoder, fmt.Sprintf("%s[%d]", location, index)); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("JSON 数组未正确结束")
		}
	default:
		return errors.New("JSON 分隔符无效")
	}
	return nil
}
