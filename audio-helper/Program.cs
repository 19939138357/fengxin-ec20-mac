using System;
using System.IO;
using System.Threading;
using NAudio.Wave;

class Program {
 static int Input(string keyword) { for(int i=0;i<WaveIn.DeviceCount;i++) if(WaveIn.GetCapabilities(i).ProductName.IndexOf(keyword,StringComparison.OrdinalIgnoreCase)>=0)return i; throw new Exception("Modem USB input not found: "+keyword); }
 static int Output(string keyword) { for(int i=0;i<WaveOut.DeviceCount;i++) if(WaveOut.GetCapabilities(i).ProductName.IndexOf(keyword,StringComparison.OrdinalIgnoreCase)>=0)return i; throw new Exception("Modem USB output not found: "+keyword); }
 static int Main(string[] args) { try {
  string mode=args.Length>0?args[0]:"list";string keyword=args.Length>1?args[1]:"AC Interface";
  if(mode=="list") {for(int i=0;i<WaveIn.DeviceCount;i++)Console.WriteLine("IN "+i+" "+WaveIn.GetCapabilities(i).ProductName);for(int i=0;i<WaveOut.DeviceCount;i++)Console.WriteLine("OUT "+i+" "+WaveOut.GetCapabilities(i).ProductName);return 0;}
  var format=new WaveFormat(8000,16,1);
  if(mode=="capture"||mode=="probe"){
   using(var capture=new WaveInEvent()){
    capture.DeviceNumber=Input(keyword);capture.WaveFormat=format;capture.BufferMilliseconds=20;capture.NumberOfBuffers=4;
    long bytes=0;var ended=new ManualResetEvent(false);Exception failed=null;Stream stdout=Console.OpenStandardOutput();
    capture.DataAvailable+=(s,e)=>{try{if(mode=="capture"){stdout.Write(e.Buffer,0,e.BytesRecorded);stdout.Flush();}Interlocked.Add(ref bytes,e.BytesRecorded);}catch(Exception ex){failed=ex;ended.Set();}};
    capture.RecordingStopped+=(s,e)=>{if(e.Exception!=null)failed=e.Exception;ended.Set();};
    capture.StartRecording();if(mode=="capture")Console.Error.WriteLine("READY");
    if(mode=="probe"){
     using(var output=new WaveOutEvent()) { output.DeviceNumber=Output(keyword);var queue=new BufferedWaveProvider(format){ReadFully=true};output.Init(queue);output.Play();Thread.Sleep(1000);output.Stop(); }
     capture.StopRecording();if(failed!=null)throw failed;if(bytes==0)throw new Exception("No USB PCM bytes captured");Console.WriteLine("PCM probe OK: 8000 Hz mono S16_LE; capture bytes="+bytes+"; playback opened");
    }else{ended.WaitOne();capture.StopRecording();if(failed!=null)throw failed;}
   } return 0;
  }
  if(mode=="playback") {
   using(var output=new WaveOutEvent()) {output.DeviceNumber=Output(keyword);output.DesiredLatency=80;var queue=new BufferedWaveProvider(format){BufferDuration=TimeSpan.FromSeconds(2),ReadFully=true};output.Init(queue);output.Play();Console.Error.WriteLine("READY");
    var input=Console.OpenStandardInput();var buf=new byte[320];int n;while((n=input.Read(buf,0,buf.Length))>0){while(queue.BufferedBytes>format.AverageBytesPerSecond/2)Thread.Sleep(10);queue.AddSamples(buf,0,n);}output.Stop();
   } return 0;
  } throw new Exception("Unknown mode");
 }catch(Exception e){Console.Error.WriteLine(e.Message);return 1;} }
}
