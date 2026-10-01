// Package service 是业务逻辑层：鉴权、文件树操作、秒传判定、分享语义都落在这里。
//
// 分层约定：
//
//	handler（HTTP、参数校验、状态码）
//	  → service（业务规则、事务边界）
//	      → repository（SQL）
//	      → storage（内容字节）
//
// D1 开始填充。
package service
