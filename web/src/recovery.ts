// A successful TCP/SSE open is not evidence of a stable subscription. Keep the
// retry budget until the connection has remained open for thirty seconds.
export class EventRecovery {
 private failures=0;
 private retryTimer:ReturnType<typeof setTimeout>|undefined;
 private stableTimer:ReturnType<typeof setTimeout>|undefined;
 private disposed=false;
 constructor(private exhausted:()=>void){}
 schedule(retry:()=>void):number|null {
  clearTimeout(this.stableTimer);clearTimeout(this.retryTimer);
  if(this.disposed)return null;
  const delays=[1000,2000,4000,8000,15000];
  if(this.failures>=delays.length){this.exhausted();return null;}
  const delay=delays[this.failures++];
  this.retryTimer=setTimeout(()=>{this.retryTimer=undefined;if(!this.disposed)retry();},delay);
  return this.failures;
 }
 opened(){if(this.disposed)return;clearTimeout(this.stableTimer);this.stableTimer=setTimeout(()=>{this.failures=0;this.stableTimer=undefined;},30000);}
 dispose(){this.disposed=true;clearTimeout(this.retryTimer);clearTimeout(this.stableTimer);}
}
