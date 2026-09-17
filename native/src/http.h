#pragma once
#include <functional>
#include <string>
struct HttpResult { unsigned status = 0; std::string body; };
// Only loopback is accepted. File requests read at most 64 KiB per iteration.
HttpResult Request(const std::wstring& endpoint, const std::wstring& token, const std::wstring& method,
                   const std::wstring& path, const std::string& body = {}, const std::wstring& file = {},
                   std::function<void(unsigned)> progress = {});
std::string JSONString(const std::string& text);
