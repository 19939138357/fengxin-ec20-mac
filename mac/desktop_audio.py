"""Computer microphone/speaker <-> EC20, using explicit devices only."""
import contextlib
import json
import sys
import threading
import warnings
with warnings.catch_warnings():
    warnings.simplefilter('ignore',DeprecationWarning)
    import audioop
import sounddevice as sd


def devices():
    result={'inputs':[],'outputs':[]}
    for d in sd.query_devices():
        if d['name'] in ('AC Interface','AS Interface'):continue
        item={'name':d['name'],'sample_rate':d['default_samplerate']}
        if d['max_input_channels']>0:result['inputs'].append(item)
        if d['max_output_channels']>0:result['outputs'].append(item)
    return result


def exact(name,direction):
    matches=[(i,d) for i,d in enumerate(sd.query_devices()) if d['name']==name and d[f'max_{direction}_channels']>0]
    if len(matches)!=1:raise RuntimeError(f'找不到唯一的{direction}设备：{name}，请检查耳麦连接')
    return matches[0]


def bridge(config):
    modem_in,modem_out=config['modem'].split('|')
    if config['input'] in (modem_in,modem_out) or config['output'] in (modem_in,modem_out):
        raise RuntimeError('电脑麦克风和扬声器不能选择 EC20 自身接口')
    mi,_=exact(modem_in,'input');mo,_=exact(modem_out,'output')
    hi,host_in=exact(config['input'],'input');ho,host_out=exact(config['output'],'output')
    irate=int(host_in['default_samplerate']);orate=int(host_out['default_samplerate'])
    stop=threading.Event();mute=threading.Event();failures=[]
    def control():
        try:
            for line in sys.stdin:
                value=json.loads(line)
                if value.get('mute'):mute.set()
                else:mute.clear()
        finally:stop.set()
    def pump(source,sink,rate_in,rate_out,frames,is_mic):
        state=None
        try:
            while not stop.is_set():
                raw,_=source.read(frames);raw=bytes(raw)
                if is_mic and mute.is_set():raw=bytes(len(raw))
                if rate_in!=rate_out:raw,state=audioop.ratecv(raw,2,1,rate_in,rate_out,state)
                sink.write(raw)
        except Exception as exc:
            if not stop.is_set():failures.append(str(exc));stop.set()
    with contextlib.ExitStack() as stack:
        opts={'channels':1,'dtype':'int16'}
        modem_capture=stack.enter_context(sd.RawInputStream(device=mi,samplerate=8000,blocksize=160,**opts))
        modem_playback=stack.enter_context(sd.RawOutputStream(device=mo,samplerate=8000,blocksize=160,**opts))
        host_capture=stack.enter_context(sd.RawInputStream(device=hi,samplerate=irate,blocksize=irate//50,**opts))
        host_playback=stack.enter_context(sd.RawOutputStream(device=ho,samplerate=orate,blocksize=orate//50,**opts))
        threading.Thread(target=control,daemon=True).start()
        threads=[threading.Thread(target=pump,args=(modem_capture,host_playback,8000,orate,160,False),daemon=True),
                 threading.Thread(target=pump,args=(host_capture,modem_playback,irate,8000,irate//50,True),daemon=True)]
        for thread in threads:thread.start()
        print('READY',file=sys.stderr,flush=True)
        stop.wait()
        if failures:raise RuntimeError(failures[0])
