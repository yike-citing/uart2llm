#include <wx/wx.h>
#include <wx/notebook.h>
#include <wx/spinctrl.h>
#include <wx/filedlg.h>
#include <wx/filename.h>
#include <wx/stdpaths.h>
#include <windows.h>
#include <wincred.h>
#include <atomic>
#include <memory>
#include <mutex>
#include <thread>
#include <fstream>
#include <filesystem>
#include <regex>
#include "http.h"

wxDECLARE_EVENT(EVT_RESPONSE,wxThreadEvent);
wxDEFINE_EVENT(EVT_RESPONSE,wxThreadEvent);
struct Mailbox {std::mutex lock;wxEvtHandler* receiver=nullptr;};
class Console:public wxFrame {
  std::shared_ptr<Mailbox> mailbox=std::make_shared<Mailbox>();
  wxTextCtrl *endpoint,*token,*port,*username,*password,*patch,*state,*config,*schema,*logs,*result,*upstreamKey,*apiToken;
  wxSpinCtrl* baud;wxCheckBox* flow;wxGauge* gauge;wxTimer timer;
  wxStaticText *welcomeStatus,*transportNote;bool nativeUSB=false;
  std::atomic<bool> pending{false};bool polling=false;int startupAttempts=0;
  void Emit(const std::shared_ptr<Mailbox>& box,int id,const wxString& message){std::lock_guard<std::mutex> guard(box->lock);if(box->receiver){auto event=new wxThreadEvent(EVT_RESPONSE,id);event->SetString(message);wxQueueEvent(box->receiver,event);}}
  static wxTextCtrl* Text(wxWindow* panel,wxSizer* sizer,const wxString& label,const wxString& value={},long style=0){sizer->Add(new wxStaticText(panel,wxID_ANY,label),0,wxTOP|wxBOTTOM,5);auto input=new wxTextCtrl(panel,wxID_ANY,value,wxDefaultPosition,wxDefaultSize,style);sizer->Add(input,0,wxEXPAND|wxBOTTOM,8);return input;}
  static wxTextCtrl* Output(wxWindow* panel,wxSizer* sizer){auto text=new wxTextCtrl(panel,wxID_ANY,"",wxDefaultPosition,wxDefaultSize,wxTE_MULTILINE|wxTE_READONLY|wxTE_DONTWRAP);text->SetFont(wxFontInfo(10).Family(wxFONTFAMILY_TELETYPE));sizer->Add(text,1,wxEXPAND|wxTOP,8);return text;}
  static void Button(wxWindow* panel,wxSizer* sizer,const wxString& label,std::function<void()> callback){auto b=new wxButton(panel,wxID_ANY,label);b->Bind(wxEVT_BUTTON,[callback](wxCommandEvent&){callback();});sizer->Add(b,0,wxRIGHT|wxBOTTOM,7);}
  wxPanel* Page(wxNotebook* notebook,const wxString& title,wxBoxSizer*& sizer){auto panel=new wxPanel(notebook);auto outer=new wxBoxSizer(wxVERTICAL);sizer=new wxBoxSizer(wxVERTICAL);outer->Add(sizer,1,wxEXPAND|wxALL,15);panel->SetSizer(outer);notebook->AddPage(panel,title);return panel;}
  bool Confirm(const wxString& message){return wxMessageBox(message,"uart2llm",wxYES_NO|wxNO_DEFAULT|wxICON_WARNING,this)==wxYES;}
  void Send(const std::wstring& path,const std::wstring& method=L"GET",const std::string& body={},int target=0,const std::wstring& file={}) {
    if(pending.exchange(true)){if(target!=1)SetStatusText(L"正在完成上一项操作，请稍候。");return;}
    if(token->IsEmpty())LoadToken();
    const auto server=endpoint->GetValue().ToStdWstring(),secret=token->GetValue().ToStdWstring();auto box=mailbox;
    SetStatusText(target==1?L"正在更新设备状态…":L"正在处理操作，请稍候…");
    // Detached workers never access controls or the destroyed frame. Mailbox ownership guards event delivery.
    std::thread([box,server,secret,path,method,body,target,file]{
      auto emit=[box](int id,const wxString& msg){std::lock_guard<std::mutex> guard(box->lock);if(box->receiver){auto event=new wxThreadEvent(EVT_RESPONSE,id);event->SetString(msg);wxQueueEvent(box->receiver,event);}};
      try { auto response=Request(server,secret,method,path,body,file,[emit](unsigned value){emit(100,wxString::Format("%u",value));});
        emit(response.status>=200&&response.status<300?target:-1,wxString::Format("HTTP %u\n",response.status)+wxString::FromUTF8(response.body));
      }catch(const std::exception& e){emit(-1,wxString::FromUTF8(e.what()));}
    }).detach();
  }
  void LoadToken(){PCREDENTIALW credential=nullptr;if(CredReadW(L"uart2llm/admin-token",CRED_TYPE_GENERIC,0,&credential)){token->SetValue(wxString::FromUTF8(reinterpret_cast<const char*>(credential->CredentialBlob),credential->CredentialBlobSize));CredFree(credential);}}
  void LoadRuntime(){wxString roaming;if(!wxGetEnv("APPDATA",&roaming))return;std::ifstream input(std::filesystem::path((roaming+"/uart2llm/runtime.json").ToStdWstring()),std::ios::binary);if(!input)return;std::string json(65536,'\0');input.read(json.data(),json.size());json.resize(static_cast<size_t>(input.gcount()));std::smatch match;if(std::regex_search(json,match,std::regex(R"json("admin_listen"\s*:\s*"([^"\\]+)")json")))endpoint->SetValue("http://"+wxString::FromUTF8(match[1].str()));}
  void OpenManagement(const wxString& fragment={}){LoadRuntime();const auto address=endpoint->GetValue();if(!std::regex_match(address.ToStdString(),std::regex(R"(http://(localhost|127\.0\.0\.1|\[::1\]):[0-9]{1,5}/?)"))){wxMessageBox(L"管理地址必须为本机回环地址。请检查高级诊断中的管理地址。",L"无法打开看板",wxOK|wxICON_ERROR,this);return;}if(!wxLaunchDefaultBrowser(address+fragment))wxMessageBox(L"无法打开默认浏览器，请复制管理地址到浏览器访问。",L"打开管理看板",wxOK|wxICON_INFORMATION,this);}
  void LoadConnection(){wxString roaming;if(!wxGetEnv("APPDATA",&roaming))return;std::ifstream input(std::filesystem::path((roaming+"/uart2llm/config.json").ToStdWstring()),std::ios::binary);if(!input)return;std::string json(65536,'\0');input.read(json.data(),json.size());json.resize(static_cast<size_t>(input.gcount()));std::smatch match;if(std::regex_search(json,match,std::regex(R"json("serial_port"\s*:\s*"(COM[0-9]+)")json")))port->ChangeValue(wxString::FromUTF8(match[1].str()));if(std::regex_search(json,match,std::regex(R"json("baud"\s*:\s*([0-9]{4,7}))json"))){const auto value=std::stoi(match[1].str());if(value>=9600&&value<=3000000)baud->SetValue(value);}flow->SetValue(std::regex_search(json,std::regex(R"json("flow_control"\s*:\s*true)json")));}
  void UpdateAvailability(const wxString& response){const auto json=response.AfterFirst('\n').ToStdString(wxConvUTF8);std::smatch match;const bool known=std::regex_search(json,match,std::regex(R"json("transport"\s*:\s*"([A-Za-z0-9_]+)")json"));nativeUSB=known&&match[1].str().find("usb")!=std::string::npos;baud->Enable(!nativeUSB);flow->Enable(!nativeUSB);transportNote->SetLabel(nativeUSB?L"当前为原生 USB：UART 波特率及 RTS/CTS 不适用，已禁用。":known?L"当前为 UART：参数须与设备已生效设置一致。":L"暂未取得设备传输类型，请先刷新状态。");if(!port->IsModified()&&std::regex_search(json,match,std::regex(R"json("port"\s*:\s*"(COM[0-9]+)")json")))port->ChangeValue(wxString::FromUTF8(match[1].str()));welcomeStatus->SetLabel(known?(nativeUSB?L"后台已连接 · 原生 USB 设备状态已同步":L"后台已连接 · UART 设备状态已同步"):L"后台已就绪 · 请打开管理看板连接设备");Layout();}
  void StartDaemon(){wxFileName executable(wxStandardPaths::Get().GetExecutablePath());executable.SetFullName("uart2llm.exe");if(!executable.FileExists()){wxMessageBox(L"请将 uart2llm.exe 与本程序放在同一安装目录。",L"后台服务",wxOK|wxICON_ERROR,this);return;}const auto fullpath=executable.GetFullPath().ToStdWstring();const wchar_t* args[]={fullpath.c_str(),L"start",nullptr};long pid=wxExecute(args,wxEXEC_ASYNC|wxEXEC_HIDE_CONSOLE);if(pid>0){startupAttempts=30;polling=true;}SetStatusText(pid>0?L"后台正在启动，将自动连接，无需输入管理密码。":L"无法启动后台，请检查安装目录。");}
public:
  Console():wxFrame(nullptr,wxID_ANY,L"uart2llm · 本地模型代理",wxDefaultPosition,wxSize(1100,840)),timer(this){
    mailbox->receiver=this;CreateStatusBar();SetMinSize(wxSize(780,640));auto panel=new wxPanel(this);auto layout=new wxBoxSizer(wxVERTICAL);panel->SetSizer(layout);
    auto controls=new wxBoxSizer(wxHORIZONTAL);Button(panel,controls,L"刷新状态",[this]{LoadToken();LoadRuntime();polling=true;Send(L"/state",L"GET",{},1);});Button(panel,controls,L"启动后台服务",[this]{StartDaemon();});Button(panel,controls,L"停止后台服务…",[this]{if(Confirm(L"停止代理服务？正在进行的模型请求将被中断。")){polling=false;Send(L"/shutdown",L"POST");}});layout->Add(controls,0,wxALL,12);
    auto sections=new wxNotebook(panel,wxID_ANY);layout->Add(sections,1,wxEXPAND|wxLEFT|wxRIGHT|wxBOTTOM,12);
    wxBoxSizer* homeSizer;auto home=Page(sections,L"欢迎使用",homeSizer);
    auto heading=new wxStaticText(home,wxID_ANY,L"让你的应用，通过 ESP32 连接模型服务");heading->SetFont(wxFontInfo(21).Bold());homeSizer->Add(heading,0,wxTOP|wxBOTTOM,22);
    auto intro=new wxStaticText(home,wxID_ANY,L"管理看板提供清晰的设备状态、模型用量与引导式配置。\n打开即可使用，无需输入管理密码或 Token。");intro->SetFont(wxFontInfo(12));homeSizer->Add(intro,0,wxBOTTOM,24);
    auto open=new wxButton(home,wxID_ANY,L"打开管理看板",wxDefaultPosition,wxSize(240,48));open->SetFont(wxFontInfo(13).Bold());open->Bind(wxEVT_BUTTON,[this](wxCommandEvent&){OpenManagement();});homeSizer->Add(open,0,wxBOTTOM,24);
    auto shortcuts=new wxBoxSizer(wxHORIZONTAL);Button(home,shortcuts,L"接入应用",[this]{OpenManagement(L"#apps");});Button(home,shortcuts,L"设备与网络",[this]{OpenManagement(L"#device");});Button(home,shortcuts,L"服务设置",[this]{OpenManagement(L"#settings");});homeSizer->Add(shortcuts,0,wxBOTTOM,24);
    welcomeStatus=new wxStaticText(home,wxID_ANY,L"正在自动连接本机后台…");welcomeStatus->SetFont(wxFontInfo(12).Bold());homeSizer->Add(welcomeStatus,0,wxBOTTOM,20);
    auto note=new wxStaticText(home,wxID_ANY,L"关闭本窗口不会停止代理服务，模型请求继续运行。\n需要离线检查完整配置目录、日志或固件维护时，可使用「高级诊断」。\n首次使用请在管理看板选择设备、连接 Wi-Fi，再设置上游服务。");homeSizer->Add(note,0,wxTOP,10);
    wxBoxSizer* advancedSizer;auto advanced=Page(sections,L"高级诊断",advancedSizer);
    advancedSizer->Add(new wxStaticText(advanced,wxID_ANY,L"面向维护人员的完整目录与原始数据。日常设置请使用管理看板；机密字段不要从诊断结果复制回配置。"),0,wxBOTTOM,8);
    auto top=new wxBoxSizer(wxHORIZONTAL);auto auth=new wxBoxSizer(wxVERTICAL);endpoint=Text(advanced,auth,L"本机管理地址","http://localhost:8766");top->Add(auth,1,wxRIGHT,12);auto credentials=new wxBoxSizer(wxVERTICAL);token=Text(advanced,credentials,L"高级凭据覆盖（已自动读取 Windows 保存值，无需填写）","",wxTE_PASSWORD);top->Add(credentials,1);advancedSizer->Add(top,0,wxEXPAND|wxBOTTOM,8);
    auto notebook=new wxNotebook(advanced,wxID_ANY);advancedSizer->Add(notebook,1,wxEXPAND);
    wxBoxSizer* s;auto page=Page(notebook,L"状态快照",s);auto row=new wxBoxSizer(wxHORIZONTAL);Button(page,row,L"刷新快照",[this]{Send(L"/state",L"GET",{},1);});Button(page,row,L"硬件能力",[this]{Send(L"/capabilities");});Button(page,row,L"任务栈遥测",[this]{Send(L"/tasks");});s->Add(row);s->Add(new wxStaticText(page,wxID_ANY,L"每秒更新状态；不可用的值按设备实际报告显示。"));state=Output(page,s);
    page=Page(notebook,L"设备连接",s);port=Text(page,s,L"设备端口（从当前配置自动读取）","");transportNote=new wxStaticText(page,wxID_ANY,L"连接后自动识别实际传输方式。UART 参数不适用于原生 USB。");s->Add(transportNote,0,wxBOTTOM,10);s->Add(new wxStaticText(page,wxID_ANY,L"UART 波特率（8N1）"));baud=new wxSpinCtrl(page,wxID_ANY,"115200",wxDefaultPosition,wxDefaultSize,wxSP_ARROW_KEYS,9600,3000000,115200);s->Add(baud,0,wxBOTTOM,10);flow=new wxCheckBox(page,wxID_ANY,L"RTS/CTS 硬件流控（仅 UART）");s->Add(flow,0,wxBOTTOM,10);row=new wxBoxSizer(wxHORIZONTAL);Button(page,row,L"列出端口",[this]{Send(L"/ports");});Button(page,row,L"连接设备",[this]{if(port->IsEmpty()){wxMessageBox(L"请先选择设备端口，或在管理看板中使用引导式连接。",L"选择设备",wxOK|wxICON_INFORMATION,this);return;}Send(L"/connect",L"POST","{\"port\":"+JSONString(port->GetValue().ToStdString(wxConvUTF8))+",\"baud\":"+std::to_string(nativeUSB?115200:baud->GetValue())+",\"flow_control\":"+(!nativeUSB&&flow->GetValue()?"true":"false")+"}");});Button(page,row,L"断开设备",[this]{Send(L"/disconnect",L"POST");});s->Add(row);username=Text(page,s,L"配对用户名");password=Text(page,s,L"配对密码","",wxTE_PASSWORD);Button(page,s,L"安全配对",[this]{const auto body="{\"username\":"+JSONString(username->GetValue().ToStdString(wxConvUTF8))+",\"password\":"+JSONString(password->GetValue().ToStdString(wxConvUTF8))+"}";Send(L"/pair",L"POST",body);password->Clear();});s->Add(new wxStaticText(page,wxID_ANY,L"配对凭据在首次烧录时生成，已配对设备自动使用保存值。"),0,wxTOP,12);
    page=Page(notebook,L"配置事务",s);s->Add(new wxStaticText(page,wxID_ANY,L"高级配置事务：仅提交 host/device 下需要变更的字段。请勿提交已脱敏的机密占位内容。"));patch=new wxTextCtrl(page,wxID_ANY,"{\n  \"device\": {}\n}",wxDefaultPosition,wxSize(-1,150),wxTE_MULTILINE);s->Add(patch,0,wxEXPAND|wxTOP|wxBOTTOM,10);row=new wxBoxSizer(wxHORIZONTAL);Button(page,row,L"校验并应用",[this]{Send(L"/config",L"PATCH",patch->GetValue().ToStdString(wxConvUTF8));patch->SetValue("{\n  \"device\": {}\n}");});Button(page,row,L"确认生效",[this]{Send(L"/config/confirm",L"POST");});Button(page,row,L"回滚配置",[this]{Send(L"/config/rollback",L"POST");});Button(page,row,L"读取配置",[this]{Send(L"/config",L"GET",{},2);});s->Add(row);config=Output(page,s);
    page=Page(notebook,L"完整字段目录",s);Button(page,s,L"读取完整配置定义",[this]{Send(L"/config/schema",L"GET",{},3);});Button(page,s,L"读取完整状态定义",[this]{Send(L"/state/schema",L"GET",{},3);});schema=Output(page,s);
    page=Page(notebook,L"访问凭据",s);s->Add(new wxStaticText(page,wxID_ANY,L"上游密钥保存于 Windows 凭据管理器。日常接入请使用管理看板。"),0,wxBOTTOM,12);upstreamKey=Text(page,s,L"上游 API 密钥","",wxTE_PASSWORD);Button(page,s,L"保存上游密钥",[this]{if(upstreamKey->IsEmpty())return;Send(L"/credentials",L"POST","{\"upstream_key\":"+JSONString(upstreamKey->GetValue().ToStdString(wxConvUTF8))+"}");upstreamKey->Clear();});Button(page,s,L"显示本地 API 密钥",[this]{Send(L"/api-token",L"GET",{},5);});apiToken=Text(page,s,L"本地 API 密钥（仅手动显示）","",wxTE_READONLY);Button(page,s,L"隐藏密钥",[this]{apiToken->Clear();});s->Add(new wxStaticText(page,wxID_ANY,L"设备配置使用字段目录中的点分键名，例如 wifi.ssid。"),0,wxTOP,20);
    page=Page(notebook,L"日志与诊断",s);row=new wxBoxSizer(wxHORIZONTAL);Button(page,row,L"读取日志",[this]{Send(L"/logs",L"GET",{},4);});Button(page,row,L"扫描 Wi-Fi",[this]{Send(L"/diagnostics",L"POST","{\"action\":\"wifi_scan\"}",4);});Button(page,row,L"网络诊断",[this]{Send(L"/diagnostics",L"POST","{\"action\":\"network\"}",4);});Button(page,row,L"诊断导出",[this]{Send(L"/diagnostics",L"POST","{\"action\":\"export\"}",4);});Button(page,row,L"崩溃记录导出",[this]{Send(L"/diagnostics",L"POST","{\"action\":\"crash_export\"}",4);});Button(page,row,L"保存显示内容",[this]{wxFileDialog file(this,L"保存诊断",{},"uart2llm-diagnostics.txt","Text (*.txt)|*.txt",wxFD_SAVE|wxFD_OVERWRITE_PROMPT);if(file.ShowModal()==wxID_OK&&!logs->SaveFile(file.GetPath()))wxMessageBox(L"无法保存诊断文件。");});s->Add(row);logs=Output(page,s);
    page=Page(notebook,L"固件与维护",s);s->Add(new wxStaticText(page,wxID_ANY,L"通过当前设备连接上传 ESP32-S3 固件。升级期间请保持设备供电与连接。"),0,wxBOTTOM,15);Button(page,s,L"选择并上传固件…",[this]{wxFileDialog file(this,L"ESP32-S3 固件镜像",{}, {},"Firmware (*.bin)|*.bin",wxFD_OPEN|wxFD_FILE_MUST_EXIST);if(file.ShowModal()==wxID_OK&&Confirm(L"上传并安装所选固件？请保持设备供电。")){gauge->SetValue(0);Send(L"/firmware",L"POST",{},0,file.GetPath().ToStdWstring());}});gauge=new wxGauge(page,wxID_ANY,100);s->Add(gauge,0,wxEXPAND|wxTOP|wxBOTTOM,15);s->Add(new wxStaticText(page,wxID_ANY,L"进度表示上传至本机后台；最终完成状态以固件响应为准。"),0,wxBOTTOM,15);Button(page,s,L"重启设备…",[this]{if(Confirm(L"重启 ESP32？正在进行的模型请求将被中断。"))Send(L"/device/action",L"POST","{\"action\":\"reboot\"}");});Button(page,s,L"清除崩溃数据…",[this]{if(Confirm(L"永久清除已保存的崩溃记录？如需保留，请先导出。"))Send(L"/device/action",L"POST","{\"action\":\"clear_crash\"}");});Button(page,s,L"恢复出厂配置…",[this]{if(Confirm(L"清除设备配置并断开连接？"))Send(L"/device/action",L"POST","{\"action\":\"factory_reset\"}");});s->Add(new wxStaticText(page,wxID_ANY,L"关闭窗口不会停止后台代理。"),0,wxTOP,20);
    page=Page(notebook,L"操作结果",s);result=Output(page,s);
    Bind(EVT_RESPONSE,[this](wxThreadEvent& event){const auto id=event.GetId();if(id==100){long value=0;event.GetString().ToLong(&value);gauge->SetValue(static_cast<int>(value));return;}pending=false;const auto text=event.GetString();if(id==1){state->SetValue(text);UpdateAvailability(text);}else if(id==2)config->SetValue(text);else if(id==3)schema->SetValue(text);else if(id==4)logs->SetValue(text);else if(id==5)apiToken->SetValue(text.AfterFirst('\n'));if(id!=1&&id!=5)result->SetValue(text);if(id==1)startupAttempts=0;if(id<0){if(startupAttempts==0)polling=false;welcomeStatus->SetLabel(L"后台暂不可用或操作失败，请启动服务或打开高级诊断查看详情。");SetStatusText(L"操作未完成，请查看高级诊断中的操作结果。");}else SetStatusText(L"已更新 "+wxDateTime::Now().FormatISOTime());});
    Bind(wxEVT_TIMER,[this](wxTimerEvent&){if(polling&&!pending){if(startupAttempts>0){--startupAttempts;if(token->IsEmpty())LoadToken();LoadRuntime();}Send(L"/state",L"GET",{},1);}});timer.Start(1000);
    Bind(wxEVT_CLOSE_WINDOW,[this](wxCloseEvent& event){if(pending&&event.CanVeto()&&!Confirm(L"操作仍在进行，确定关闭窗口？后台代理继续运行，本次管理操作的显示可能中断。")){event.Veto();return;}Destroy();});
    LoadToken();LoadRuntime();LoadConnection();polling=true;SetStatusText(L"正在自动读取设备状态，无需登录。关闭窗口后代理继续运行。");Centre();
  }
  ~Console() override {timer.Stop();std::lock_guard<std::mutex> guard(mailbox->lock);mailbox->receiver=nullptr;}
};
class Application:public wxApp {public: bool OnInit() override{auto frame=new Console();frame->Show();return true;}};
wxIMPLEMENT_APP(Application);
