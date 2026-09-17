export type Obj = Record<string, any>;
export const object = (v: unknown): Obj => v && typeof v === 'object' && !Array.isArray(v) ? v as Obj : {};
export const number = (v: unknown): number | null => typeof v === 'number' && Number.isFinite(v) ? v : null;
export function scaledInput(raw:string,scale:number){if(raw==='')return null;const n=Number(raw)*scale,rounded=Math.round(n);return Math.abs(n-rounded)<1e-7?rounded:n;}
// ESP-IDF 6.0.1: esp_system.h and esp_flash_partitions.h. Unknown values must
// remain distinguishable from a healthy boot/confirmed image.
export function resetReason(value:unknown){const labels=['原因未知','上电启动','外部复位引脚','软件重启','程序异常重启','中断看门狗重启','任务看门狗重启','看门狗重启','深度睡眠唤醒','电压过低重启','SDIO 复位','USB 复位','JTAG 复位','eFuse 错误复位','电源波动复位','CPU 锁死复位'];return number(value)===null?'未读取':labels[Number(value)]||`未知（${value}）`;}
export function imageState(value:unknown){const labels:Record<number,string>={0:'新镜像，等待首次启动',1:'等待启动自检确认',2:'已确认有效',3:'镜像无效',4:'验证中止，已放弃启动',4294967295:'未定义（无状态标记）'};return number(value)===null?'未读取':labels[Number(value)]||`未知（${value}）`;}
export function signalQuality(value:unknown){const v=number(value);return v===null?'未采集':v>=-67?'良好':v>=-75?'一般':'较弱';}
export const fmt = (v: unknown, digits = 0) => number(v) === null ? '—' : Number(v).toLocaleString('zh-CN', {maximumFractionDigits: digits});
export function bytes(v: unknown) {const n = number(v); if(n === null) return '—'; return n >= 1048576 ? `${fmt(n/1048576,1)} MiB` : `${fmt(n/1024,1)} KiB`;}
export function transport(s: Obj) {return s.device?.uart?.active === false || String(s.device?.transport || '').toLowerCase().includes('usb') ? 'USB' : s.device?.uart?.active === true ? 'UART' : '未知';}
export function fresh(s: Obj, now = Date.now(), interval = 1000) {const age = now - Date.parse(s.sampled_at || ''), deviceAge=now-(s._deviceObservedAt??Date.parse(s.sampled_at||'')); return Boolean(!s._feedUnavailable && s.connected && s.paired && s.device && Number.isFinite(age) && age >= -1000 && age < Math.max(5000, interval*3) && deviceAge < Math.max(5000, interval*3) && (s.connection?.link?.session ?? s.link?.link?.session) === s.device.session);}
export function countdown(config: Obj, s: Obj, now = Date.now()) {if(!config.device?.pending || !fresh(s,now)||config.device.session!==s.device?.session) return null; const deadline=number(config.device.confirm_deadline_ms),current=number(config.device.current_time_ms),received=number(config._receivedAt);if(deadline===null||current===null||received===null||received>now)return null;return Math.max(0,Math.min(30,Math.ceil((deadline-current-(now-received))/1000)));}
export type Point = {time:number;sourceTime:number; session:unknown; sample:number|null; tx:number|null; rx:number|null; down:number|null; up:number|null; memory:number|null; requests:number|null};
export function sampleHistory(history:Point[], s:Obj, now = Date.now(), interval=1000):Point[] {
  const valid=fresh(s,now,interval), dev=s.device, sample=valid?number(dev.sample_time_ms):null, previous=history.at(-1);
  const sourceTime=Date.parse(s.sampled_at)||now;
  if(previous && previous.sourceTime === sourceTime && ((valid&&previous.sample!==null)||(!valid&&previous.sample===null))) return history;
  const tx=valid?number(dev.link?.tx_bytes):null, rx=valid?number(dev.link?.rx_bytes):null;
  const same=valid && previous && previous.session===dev.session && sample!==null && previous.sample!==null && sample>previous.sample && now-previous.time<10000;
  const delta=same?(sample!-previous!.sample!)/1000:0;
  const rate=(a:number|null,b:number|null|undefined) => same && a!==null && b!==null && b!==undefined && a>=b ? (a-b)/delta/1024 : null;
  const point:Point={time:valid?sourceTime:now,sourceTime,session:valid?dev.session:null,sample,tx,rx,down:rate(tx,previous?.tx),up:rate(rx,previous?.rx),memory:valid?number(dev.memory?.internal_free):null,requests:!s._feedUnavailable&&now-sourceTime<5000?number(s.llm?.requests_last_60s):null};
  return [...history.filter(p=>now-p.time<=300000),point].slice(-300);
}
export type Field = {key:string; label:string; hint:string; type?:'boolean'|'number'|'password'; min?:number; max?:number; unit?:string; scale?:number; options?:[string, string][]};
export const hostFields:Field[] = [
 {key:'upstream_url',label:'模型服务地址',hint:'服务商提供的 HTTPS 地址，例如 https://api.deepseek.com。后续请求生效。'},
 {key:'max_concurrent',label:'同时处理请求',hint:'最多 4 路；达到上限时新请求会提示忙碌，不取消已有回答。',type:'number',min:1,max:4,unit:'路'},
 {key:'connect_timeout_seconds',label:'建立连接等待',hint:'连接与 TLS 握手的等待上限，不限制回答总时长。',type:'number',min:1,max:600,unit:'秒'},
 {key:'header_timeout_seconds',label:'等待开始响应',hint:'等待上游响应头；不是首 Token 耗时。',type:'number',min:1,max:3600,unit:'秒'},
 {key:'idle_timeout_seconds',label:'无数据等待',hint:'只有持续没有数据传输时才超时。',type:'number',min:1,max:86400,unit:'秒'},
 {key:'api_listen',label:'本机 API 监听地址',hint:'仅回环地址；例如 localhost:8765。保存后重启后台生效。'},
 {key:'admin_listen',label:'管理页面监听地址',hint:'例如 localhost:8766；不能与 API 相同。重启前仍访问当前地址。'},
 {key:'serial_port',label:'设备端口',hint:'通过设备连接表单检测、连接并保存。'},
 {key:'baud',label:'连接波特率',hint:'仅 UART 的电脑打开参数。',type:'number',min:9600,max:3000000},
 {key:'flow_control',label:'连接硬件流控',hint:'仅 UART，需接入 RTS/CTS。',type:'boolean'}
];
export const deviceFields:Field[] = [
 {key:'wifi.ssid',label:'Wi-Fi 名称',hint:'选择附近网络，也可输入隐藏网络名称。最多 32 个 UTF-8 字节。'},
 {key:'wifi.password',label:'Wi-Fi 密码',hint:'留存的密码不会回读；只有明确选择更换或开放网络时才修改。',type:'password'},
 {key:'wifi.hostname',label:'设备网络名称',hint:'局域网主机名，1–32 个字母、数字或连字符。'},
 {key:'ip.dhcp',label:'自动获取网络地址',hint:'推荐保持开启；关闭后填写完整 IPv4 地址。',type:'boolean'},
 {key:'ip.address',label:'设备 IPv4 地址',hint:'例如 192.168.1.50，仅手动地址模式生效。'},
 {key:'ip.gateway',label:'网关',hint:'通常为路由器地址，仅手动模式生效。'},
 {key:'ip.netmask',label:'子网掩码',hint:'例如 255.255.255.0，仅手动模式生效。'},
 {key:'ip.dns',label:'DNS 服务器',hint:'用于 ESP32 解析上游域名，仅手动模式生效。'},
 {key:'uart.baud',label:'设备 UART 波特率',hint:'双端切换后须验证并确认；2 Mbps 填写 2000000。',type:'number',min:9600,max:3000000},
 {key:'uart.flow_control',label:'RTS/CTS 硬件流控',hint:'仅固件配置了对应引脚且实际接线时启用。',type:'boolean'},
 {key:'tcp.connect_timeout_ms',label:'设备 TCP 连接等待',hint:'设备端等待上游 TCP 建连，与电脑超时独立。',type:'number',min:1000,max:120000,scale:1000,unit:'秒'},
 {key:'tcp.idle_timeout_ms',label:'设备 TCP 无数据等待',hint:'设备端关闭长时间没有活动的 TCP 连接。',type:'number',min:1000,max:86400000,scale:1000,unit:'秒'},
 {key:'telemetry.interval_ms',label:'设备状态采样间隔',hint:'电脑约每秒获取一次快照，更快采样不会提高页面刷新率。',type:'number',min:250,max:60000,scale:1000,unit:'秒'},
 {key:'logs.level',label:'设备日志详细程度',hint:'调试时再提高；不改变事件列表的保留容量。',type:'number',min:0,max:5,options:[['0','关闭'],['1','错误'],['2','警告'],['3','信息（推荐）'],['4','调试'],['5','详细']]}
];
export function validateField(f:Field, v:unknown):string {
 if(f.type==='boolean') return typeof v==='boolean'?'':'请选择开关状态';
 if(f.type==='number') return number(v)!==null && Number.isInteger(v) && Number(v)>=(f.min??-Infinity) && Number(v)<=(f.max??Infinity)?'':`请输入 ${(f.min??0)/(f.scale??1)}–${(f.max??0)/(f.scale??1)} ${f.unit||''}内的数值`;
 if(typeof v!=='string') return '请填写此项';
 if(f.key==='upstream_url') {try {const u=new URL(v);if(u.protocol!=='https:'||!u.hostname||u.username||u.password||u.search||u.hash||v.includes('..')) return '请使用不含凭据、查询参数和 .. 的 HTTPS 地址';}catch{return '请输入完整的 HTTPS 地址';}}
 if(f.key==='wifi.ssid' && (!v || new TextEncoder().encode(v).length>32)) return '请输入 1–32 字节的网络名称';
 if(f.key==='wifi.hostname' && !/^[a-zA-Z0-9-]{1,32}$/.test(v)) return '仅支持 1–32 位字母、数字和连字符';
 if(f.key==='wifi.password') {const n=new TextEncoder().encode(v).length;if(n!==0 && !(n>=8&&n<=63) && !/^[a-fA-F0-9]{64}$/.test(v)) return '请输入 8–63 字节密码或 64 位十六进制密钥';}
 if(f.key.startsWith('ip.') && f.key!=='ip.dhcp' && !/^(\d{1,3}\.){3}\d{1,3}$/.test(v)) return '请输入 IPv4 地址';
 if(f.key.startsWith('ip.') && f.key!=='ip.dhcp' && v.split('.').some(x=>Number(x)>255 || (x.length>1&&x[0]==='0'))) return 'IPv4 每段应为 0–255，不含前导零';
 if(f.key.endsWith('_listen')) {const m=/^(localhost|127(?:\.\d{1,3}){3}|\[::1\]):([0-9]+)$/.exec(v);if(!m || Number(m[2])<1 || Number(m[2])>65535 || m[1].startsWith('127') && m[1].split('.').some(x=>Number(x)>255)) return '请填写 localhost:端口、127.x.x.x:端口或 [::1]:端口';}
 return '';
}
export function buildPatch(fields:Field[], draft:Obj, baseline:Obj) {const patch:Obj={}, errors:Obj={};for(const f of fields){if(!(f.key in draft)||Object.is(draft[f.key],baseline[f.key])) continue;const error=validateField(f,draft[f.key]);if(error)errors[f.key]=error;else patch[f.key]=draft[f.key];}return {patch,errors};}
export function passwordEdit(mode:string,value:unknown):{value?:string;error?:string} {if(mode==='keep')return {};if(mode==='open')return {value:''};if(typeof value!=='string'||!value)return {error:'更换密码时请填写新密码；开放网络请选择对应选项。'};const error=validateField(deviceFields.find(f=>f.key==='wifi.password')!,value);return error?{error}:{value};}
export type Change={key:string;label:string;before:string;after:string};
export function changeSummary(fields:Field[],draft:Obj,baseline:Obj,passwordMode='keep'):Change[]{
 const describe=(f:Field,v:unknown)=>{if(v===undefined)return '未读取';if(v===null||v==='')return '未填写';if(f.type==='boolean')return v===true?'开启':'关闭';const option=f.options?.find(([value])=>value===String(v));if(option)return option[1];if(f.type==='number'&&number(v)!==null)return `${fmt(Number(v)/(f.scale||1),3)}${f.unit?' '+f.unit:''}`;return String(v);};
 return fields.flatMap(f=>{
  if(f.type==='password')return passwordMode==='keep'?[]:[{key:f.key,label:f.label,before:baseline[f.key]?'已设置密码':'未设置密码',after:passwordMode==='open'?'开放网络（无需密码）':'替换为新密码'}];
  if(!(f.key in draft)||Object.is(draft[f.key],baseline[f.key]))return [];
  return [{key:f.key,label:f.label,before:describe(f,baseline[f.key]),after:describe(f,draft[f.key])}];
 });
}
export function explainError(e:unknown) {const text=e instanceof Error?e.message:String(e);let msg=text;try {const parsed=JSON.parse(text);msg=parsed.error?.message||text;}catch{/* ordinary message */}if(msg.includes('finish active'))return '请等待当前模型请求结束，再修改设备设置。可先暂停接收新请求。';if(msg.includes('maintenance')||msg.includes('pending configuration'))return '设备正在维护或等待配置确认，请先完成当前操作。';if(msg.includes('Wi-Fi configuration'))return '新的 Wi-Fi 尚未取得网络地址，请检查网络名称和密码，或恢复原配置。';return msg;}
