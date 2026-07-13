package evidence

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxImageBytes  = 50 << 20
	maxImageWidth  = 16384
	maxImageHeight = 16384
	maxImagePixels = 64_000_000
)

func ValidateImageFile(path string) error {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("not a readable regular file")
	}
	if info.Size() == 0 {
		return fmt.Errorf("image file is empty")
	}
	if info.Size() > maxImageBytes {
		return fmt.Errorf("image exceeds %d MiB limit", maxImageBytes>>20)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open image: %w", err)
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(path))
	var width, height int
	switch ext {
	case ".png", ".jpg", ".jpeg":
		config, format, decodeErr := image.DecodeConfig(io.LimitReader(file, maxImageBytes+1))
		if decodeErr != nil {
			return fmt.Errorf("decode image header: %w", decodeErr)
		}
		if (ext == ".png" && format != "png") || ((ext == ".jpg" || ext == ".jpeg") && format != "jpeg") {
			return fmt.Errorf("image content does not match %s extension", ext)
		}
		width, height = config.Width, config.Height
	default:
		return fmt.Errorf("must be a png, jpg, or jpeg file")
	}
	if width <= 0 || height <= 0 || width > maxImageWidth || height > maxImageHeight || int64(width)*int64(height) > maxImagePixels {
		return fmt.Errorf("image dimensions %dx%d are outside the allowed range", width, height)
	}
	return nil
}

func SameFileContent(left, right string) (bool, error) {
	leftInfo, err := os.Stat(left)
	if err != nil {
		return false, err
	}
	rightInfo, err := os.Stat(right)
	if err != nil {
		return false, err
	}
	if os.SameFile(leftInfo, rightInfo) {
		return true, nil
	}
	leftDigest, err := fileDigest(left)
	if err != nil {
		return false, err
	}
	rightDigest, err := fileDigest(right)
	if err != nil {
		return false, err
	}
	return bytes.Equal(leftDigest[:], rightDigest[:]), nil
}

func fileDigest(path string) ([sha256.Size]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return [sha256.Size]byte{}, err
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}
