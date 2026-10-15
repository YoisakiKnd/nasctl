import { APIError, query } from './api.js';

// XMLHttpRequest exposes upload progress without buffering a File in JS memory.
export function uploadFile(root, path, file, { signal, onprogress } = {}) {
  return new Promise((resolve, reject) => {
    const request = new XMLHttpRequest();
    const abort = () => request.abort();
    const finish = (callback, value) => { signal?.removeEventListener('abort', abort); callback(value); };
    request.open('PUT',query('/api/v1/files/upload',{root,path}));
    request.withCredentials = true;
    request.timeout = 30 * 60 * 1000;
    request.setRequestHeader('Content-Type','application/octet-stream');
    request.setRequestHeader('X-Nasctl-Confirm','1');
    request.upload.onprogress = (event) => {if (event.lengthComputable) onprogress?.(event.loaded,event.total);};
    request.onload = () => {
      let body; try { body = JSON.parse(request.responseText); } catch { body = null; }
      if (request.status >= 200 && request.status < 300) finish(resolve,body);
      else {
        if (request.status === 401) window.dispatchEvent(new CustomEvent('nasctl:unauthorized'));
        finish(reject,new APIError(body?.message || `上传未完成（${request.status}）`,request.status,body?.error));
      }
    };
    request.onerror = () => finish(reject,new APIError('网络中断，请刷新文件列表确认上传结果',0,'network'));
    request.ontimeout = () => finish(reject,new APIError('上传超时，请刷新文件列表确认结果',408,'timeout'));
    request.onabort = () => finish(reject,new DOMException('上传已停止','AbortError'));
    signal?.addEventListener('abort',abort,{once:true});
    if (signal?.aborted) {finish(reject,new DOMException('上传已停止','AbortError'));return;}
    request.send(file);
  });
}
