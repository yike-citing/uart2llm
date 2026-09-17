#include "http.h"
#include <windows.h>
#include <winhttp.h>
#include <algorithm>
#include <array>
#include <fstream>
#include <filesystem>
#include <limits>
#include <stdexcept>
#include <utility>
namespace {
struct Handle { HINTERNET value; explicit Handle(HINTERNET v):value(v){if(!v)throw std::runtime_error("Windows HTTP initialization failed: " + std::to_string(GetLastError()));} ~Handle(){WinHttpCloseHandle(value);} Handle(const Handle&)=delete; };
void Check(BOOL ok) { if(!ok) throw std::runtime_error("Local management request failed (Windows error " + std::to_string(GetLastError()) + "). Check that uart2llm serve is running."); }
}
std::string JSONString(const std::string& text) {
  std::string result = "\"";
  constexpr char digits[]="0123456789abcdef";
  for (unsigned char c:text) { if(c=='"'||c=='\\'){result+='\\';result+=c;}else if(c<32){result+="\\u00";result+=digits[c>>4];result+=digits[c&15];}else result+=c; }
  return result+'"';
}
HttpResult Request(const std::wstring& endpoint,const std::wstring& token,const std::wstring& method,const std::wstring& path,const std::string& body,const std::wstring& file,std::function<void(unsigned)> progress) {
  URL_COMPONENTS url{};url.dwStructSize=sizeof(url);url.dwHostNameLength=static_cast<DWORD>(-1);url.dwUrlPathLength=static_cast<DWORD>(-1);url.dwUserNameLength=static_cast<DWORD>(-1);url.dwPasswordLength=static_cast<DWORD>(-1);url.dwExtraInfoLength=static_cast<DWORD>(-1);
  Check(WinHttpCrackUrl(endpoint.c_str(),0,0,&url));
  std::wstring host(url.lpszHostName,url.dwHostNameLength);
  if(token.find_first_of(L"\r\n")!=std::wstring::npos)throw std::runtime_error("The admin token must not contain line breaks.");
  if(url.nScheme!=INTERNET_SCHEME_HTTP||(host!=L"localhost"&&host!=L"127.0.0.1"&&host!=L"[::1]"&&host!=L"::1")||url.dwUserNameLength||url.dwPasswordLength||url.dwExtraInfoLength) throw std::runtime_error("Management address must be an HTTP loopback address without credentials or a query.");
  if(url.dwUrlPathLength&&std::wstring(url.lpszUrlPath,url.dwUrlPathLength)!=L"/")throw std::runtime_error("Use the loopback origin only, for example http://localhost:8766.");
  if(host==L"localhost")host=L"127.0.0.1"; // Never resolve an administration destination through external DNS.
  Handle session(WinHttpOpen(L"uart2llm-native/0.1",WINHTTP_ACCESS_TYPE_NO_PROXY,WINHTTP_NO_PROXY_NAME,WINHTTP_NO_PROXY_BYPASS,0));
  Check(WinHttpSetTimeouts(session.value,5000,5000,30000,file.empty()?30000:0));
  Handle connection(WinHttpConnect(session.value,host.c_str(),url.nPort,0));
  Handle request(WinHttpOpenRequest(connection.value,method.c_str(),(L"/admin/v1"+path).c_str(),nullptr,WINHTTP_NO_REFERER,WINHTTP_DEFAULT_ACCEPT_TYPES,0));
  DWORD redirects=WINHTTP_OPTION_REDIRECT_POLICY_NEVER;
  Check(WinHttpSetOption(request.value,WINHTTP_OPTION_REDIRECT_POLICY,&redirects,sizeof(redirects)));
  std::wstring headers=L"Authorization: Bearer "+token+L"\r\nContent-Type: "+(file.empty()?L"application/json":L"application/octet-stream")+L"\r\n";
  std::ifstream stream; unsigned long long size=body.size();
  if(!file.empty()){stream.open(std::filesystem::path(file),std::ios::binary);if(!stream)throw std::runtime_error("Cannot open firmware file.");stream.seekg(0,std::ios::end);auto length=stream.tellg();if(length<=0)throw std::runtime_error("Firmware file is empty or unreadable.");size=static_cast<unsigned long long>(length);if(size>16*1024*1024)throw std::runtime_error("Firmware exceeds ESP32-S3 flash capacity (16 MiB).");stream.seekg(0);}
  if(size>std::numeric_limits<DWORD>::max())throw std::runtime_error("Request exceeds Windows HTTP size limit.");
  Check(WinHttpSendRequest(request.value,headers.c_str(),static_cast<DWORD>(-1),WINHTTP_NO_REQUEST_DATA,0,static_cast<DWORD>(size),0));
  if(stream.is_open()) { std::array<char,65536> buffer{};unsigned long long sent=0;while(sent<size){auto wanted=static_cast<std::streamsize>(std::min<unsigned long long>(buffer.size(),size-sent));stream.read(buffer.data(),wanted);auto n=stream.gcount();if(n<=0)throw std::runtime_error("Firmware file changed or could not be read.");DWORD offset=0;while(offset<static_cast<DWORD>(n)){DWORD wrote=0;Check(WinHttpWriteData(request.value,buffer.data()+offset,static_cast<DWORD>(n)-offset,&wrote));if(!wrote)throw std::runtime_error("Firmware upload stopped.");offset+=wrote;}sent+=n;if(progress)progress(static_cast<unsigned>(sent*100/size));} }
  else if(!body.empty()){DWORD offset=0;while(offset<body.size()){DWORD wrote=0;Check(WinHttpWriteData(request.value,body.data()+offset,static_cast<DWORD>(body.size()-offset),&wrote));if(!wrote)throw std::runtime_error("Management request stopped.");offset+=wrote;}}
  Check(WinHttpReceiveResponse(request.value,nullptr));
  DWORD code=0,codeSize=sizeof(code);Check(WinHttpQueryHeaders(request.value,WINHTTP_QUERY_STATUS_CODE|WINHTTP_QUERY_FLAG_NUMBER,WINHTTP_HEADER_NAME_BY_INDEX,&code,&codeSize,WINHTTP_NO_HEADER_INDEX));
  HttpResult result;result.status=code;std::array<char,16384> buffer{};
  for(;;){DWORD read=0;Check(WinHttpReadData(request.value,buffer.data(),buffer.size(),&read));if(!read)break;if(result.body.size()+read>4*1024*1024)throw std::runtime_error("Management response exceeds the 4 MiB display limit.");result.body.append(buffer.data(),read);}
  return result;
}
