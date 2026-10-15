package util

// BoolPtr 返回指向 b 的指针。配置里大量字段用指针区分「未提交」与「提交 false」，
// 就地取字面量地址在 Go 里不可行，因此需要这个helper。
func BoolPtr(b bool) *bool { return &b }
