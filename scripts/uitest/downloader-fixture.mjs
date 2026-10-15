// Disposable in-memory qBittorrent protocol fixture. Never downloads anything.
import http from 'node:http';
if(!process.argv.includes('--fixture-only'))throw new Error('--fixture-only is required');
const portIndex=process.argv.indexOf('--port');const port=portIndex<0?18102:Number(process.argv[portIndex+1]);
if(!Number.isInteger(port)||port<1024||port>65535)throw new Error('invalid local fixture port');
const hash='a'.repeat(40);let state='downloading',adds=0;const controls=[];
const server=http.createServer(async(req,res)=>{
 const url=new URL(req.url,'http://127.0.0.1');let body='';
 for await(const chunk of req){body+=chunk;if(body.length>8192){res.writeHead(413);res.end();return;}}
 const form=new URLSearchParams(body),send=(value)=>{res.setHeader('Content-Type','application/json');res.end(JSON.stringify(value));};
 switch(url.pathname){
 case '/api/v2/app/version':res.end('v5.0.2');return;
 case '/api/v2/app/webapiVersion':res.end('2.11.0');return;
 case '/api/v2/auth/login':res.end('Ok.');return;
 case '/api/v2/torrents/info':send(url.searchParams.get('tag')==='nasctl-episode-1'?[{hash,state,progress:.25,save_path:'/downloads/anime/subscription-1'}]:[]);return;
 case '/api/v2/torrents/files':send([{name:'Generated-video.mp4',progress:.25,size:128}]);return;
 case '/api/v2/torrents/add':adds++;res.end('Ok.');return;
 case '/api/v2/torrents/stop':case '/api/v2/torrents/start':case '/api/v2/torrents/recheck':
   if(req.method!=='POST'||form.get('hashes')!==hash){res.writeHead(400);res.end();return;}
   controls.push(url.pathname.split('/').at(-1));if(controls.length>100)controls.shift();
   state=url.pathname.endsWith('/stop')?'stoppedDL':url.pathname.endsWith('/start')?'downloading':'checkingDL';res.end();return;
 case '/__test/stats':send({controls,adds,state});return;
 default:res.writeHead(404);res.end();
 }
});
server.listen(port,'127.0.0.1',()=>console.log(`Disposable downloader fixture on 127.0.0.1:${port}`));
for(const signal of ['SIGINT','SIGTERM'])process.on(signal,()=>server.close(()=>process.exit(0)));
