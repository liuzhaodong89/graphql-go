package main

// registryDemoQuery 同时作为实际请求文本和 ParamRegistry 的 document identity。
// 两处必须引用同一个常量，避免空格、换行或注释差异导致配置无法命中。
const registryDemoQuery = `query RegistryDemo {
  seed: seedUser {
    id
  }

  profile: userByID(id: "query-placeholder") @audit(tag: "query-tag") {
    id
    name
  }
}`
