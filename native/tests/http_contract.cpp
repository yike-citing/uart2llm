#include "http.h"
#include <iostream>
#include <stdexcept>

int main() {
  unsigned checks=0;
  auto check=[&](bool condition,const char* description){if(!condition)throw std::runtime_error(description);++checks;};
  try {
    check(JSONString("quote\"\n\\") == "\"quote\\\"\\u000a\\\\\"", "JSON credential escaping");
    check(JSONString(std::string("a\0b",3)) == "\"a\\u0000b\"", "JSON embedded null escaping");
    for(const auto* address:{L"http://example.com:8766", L"http://127.0.0.1.example.com:8766", L"https://localhost:8766", L"http://user:secret@localhost:8766", L"http://localhost:8766/admin", L"http://localhost:8766?token=secret"}) {
      bool rejected=false;
      try {Request(address,L"test-token",L"GET",L"/state");} catch(const std::runtime_error& error) {const std::string text=error.what();rejected=text.find("address")!=std::string::npos||text.find("origin")!=std::string::npos;}
      check(rejected,"Unsafe administration address must be rejected before connecting");
    }
    bool rejected=false;
    try {Request(L"http://127.0.0.1:8766",L"token\r\nX-Injected: 1",L"GET",L"/state");}catch(const std::runtime_error& error){rejected=std::string(error.what()).find("line breaks")!=std::string::npos;}
    check(rejected,"Credential header injection must be rejected before connecting");
    std::cout<<checks<<" native request boundary checks passed\n";
    return 0;
  } catch(const std::exception& error) {std::cerr<<error.what()<<'\n';return 1;}
}
