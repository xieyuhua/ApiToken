// Package util 存放被多个包共用的零依赖小工具。
//
// 只放满足以下条件的函数：纯函数、不引入第三方依赖、不属于任何单一业务包。
// 之前同类工具在三四个包里各写一份（firstNonEmpty / boolPtr / writeJSON），
// 签名还不完全一致，改动一处容易漏掉另一处。这里统一收口。
package util
