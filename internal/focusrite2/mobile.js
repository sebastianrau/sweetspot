
(() => {
'use strict';
const status=document.querySelector('#status'),video=document.querySelector('#video');
const photoInput=document.querySelector('#image'),photoFallback=document.querySelector('#photo-fallback');
const camera=document.querySelector('#camera'),stopButton=document.querySelector('#stop'),help=document.querySelector('#camera-help');
const scanner=document.querySelector('#scanner');
let stream,finished=false,submitted=false,generation=0,scannedFrames=0;
camera.hidden=false;help.textContent='Scanner ready. Tap Scan with camera or select a QR photo below.';
function report(event,message){fetch('client-event',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({event,message,secure:window.isSecureContext,camera:!!navigator.mediaDevices?.getUserMedia})}).catch(()=>{});}
window.addEventListener('error',e=>{help.textContent='Scanner error: '+e.message+'. Photo upload below still works.';report('error',e.message);});
window.addEventListener('unhandledrejection',e=>{help.textContent='Scanner error. Use the QR photo form below.';report('error',String(e.reason));});
report('ready','Mobile scanner initialized');
function stop(){document.body.classList.remove('scanning');camera.hidden=false;generation++;if(stream)stream.getTracks().forEach(t=>t.stop());stream=null;video.srcObject=null;video.hidden=true;stopButton.hidden=true;camera.disabled=finished||submitted;camera.textContent='Scan with camera';}
async function submit(body,headers,endpoint='qr'){
 const r=await fetch(endpoint,{method:'POST',body,headers});
 if(r.status===204){scannedFrames++;help.textContent='Scanning… Keep all four corners of the FC2 QR code in view.';return false;}
 if(!r.ok)throw Error(await r.text());
 submitted=true;status.textContent='Code sent. Waiting for Focusrite Control 2…';stop();return true;
}
async function manual(body,headers){try{await submit(body,headers);}catch(e){help.textContent=e.message;}}
const photo=document.querySelector('#photo'),photoState=document.querySelector('#photo-state'),photoSend=document.querySelector('#photo-send'),preview=document.querySelector('#photo-preview');
let uploading=false,previewURL;
async function uploadPhoto(){
 const file=photoInput.files?.[0];if(!file||uploading)return;
 if(finished||submitted){photoState.textContent='This pairing session has already ended or received a code. Start a new pairing in Sweetspot if needed.';return;}
 uploading=true;photoSend.disabled=true;stop();report('photo-selected','Photo selected for upload');
 try{
  photoState.textContent='Reading photo…';
  if(previewURL)URL.revokeObjectURL(previewURL);previewURL=URL.createObjectURL(file);preview.src=previewURL;preview.hidden=false;
  // Browser image decoding handles phone-photo orientation and can convert
  // formats such as HEIC to the JPEG understood by the local QR decoder.
  const img=new Image();const loaded=new Promise((resolve,reject)=>{img.onload=resolve;img.onerror=()=>reject(Error('Cannot open this photo. Select a JPEG or PNG image.'));});img.src=previewURL;await loaded;
  const scale=Math.min(1,2400/Math.max(img.naturalWidth,img.naturalHeight));const canvas=document.createElement('canvas');canvas.width=Math.round(img.naturalWidth*scale);canvas.height=Math.round(img.naturalHeight*scale);canvas.getContext('2d').drawImage(img,0,0,canvas.width,canvas.height);
  const jpeg=await new Promise(resolve=>canvas.toBlob(resolve,'image/jpeg',0.95));if(!jpeg)throw Error('Could not prepare the photo. Select a JPEG or PNG image.');
  const form=new FormData();form.append('image',jpeg,'fc2-qr.jpg');photoState.textContent='Uploading photo and reading QR code…';
  await submit(form);photoState.textContent='Photo received. Waiting for Focusrite Control 2 to confirm the code.';
 }catch(e){photoState.textContent='Photo upload failed: '+e.message;report('photo-error',e.message);}
 finally{uploading=false;photoSend.disabled=finished||submitted;}
}
photoInput.addEventListener('change',uploadPhoto);
photo.onsubmit=e=>{e.preventDefault();uploadPhoto();};
document.querySelector('#code').onsubmit=e=>{e.preventDefault();manual(document.querySelector('#payload').value,{'Content-Type':'text/plain'});};
async function poll(){
 try{const r=await fetch('status');if(!r.ok)throw Error('Pairing session unavailable');const s=await r.json();status.textContent=s.message;
 if(s.complete){if(photoState)photoState.textContent=s.message;finished=true;stop();document.querySelectorAll('button,input,textarea').forEach(e=>e.disabled=true);return;}}
 catch(e){status.textContent='Connection lost. Check Wi-Fi or start pairing again in Sweetspot.';}
 if(!finished)setTimeout(poll,1000);
}
poll();stopButton.onclick=stop;window.addEventListener('pagehide',stop);
const liveCameraAvailable=window.isSecureContext&&navigator.mediaDevices&&navigator.mediaDevices.getUserMedia;
function offerPhoto(){
 photoFallback.open=true;
 help.textContent='This browser cannot use live camera scanning with the current certificate. Take a QR photo below, then tap Send QR photo.';
 photoInput.click();
}
if(liveCameraAvailable)photoFallback.open=false;
if(!liveCameraAvailable){
 camera.textContent='Open camera for QR photo';
 help.textContent='Live scanning is unavailable in this browser. You can still photograph the FC2 QR code and send the photo.';
}
camera.onclick=async()=>{
 if(!liveCameraAvailable){offerPhoto();return;}
 stop();document.body.classList.add('scanning');photoFallback.open=false;scannedFrames=0;scanner.scrollIntoView({block:'start'});camera.disabled=true;camera.textContent='Opening camera…';stopButton.hidden=false;help.textContent='Allow camera access when the browser asks. You can also stop and use a QR photo below.';const attempt=generation;
 try{
  const opened=await navigator.mediaDevices.getUserMedia({audio:false,video:{facingMode:{ideal:'environment'},width:{ideal:1920},height:{ideal:1080}}});
  if(attempt!==generation||finished||submitted){opened.getTracks().forEach(t=>t.stop());return;}
  stream=opened;const track=stream.getVideoTracks()[0];const capabilities=track?.getCapabilities?.();if(capabilities?.focusMode?.includes('continuous'))track.applyConstraints({advanced:[{focusMode:'continuous'}]}).catch(()=>{});video.srcObject=stream;video.hidden=false;stopButton.hidden=false;await video.play();camera.hidden=true;scanner.scrollIntoView({block:'start'});help.textContent='Point the camera at the complete FC2 QR code.';
  let detector;
  if(window.BarcodeDetector){try{const formats=await BarcodeDetector.getSupportedFormats();if(formats.includes('qr_code'))detector=new BarcodeDetector({formats:['qr_code']});}catch(e){}}
  const canvas=document.createElement('canvas');const context=canvas.getContext('2d');
  async function scan(){
   if(attempt!==generation||!stream||finished||submitted)return;
   try{
    if(video.readyState>=2&&video.videoWidth){
     if(detector){try{const codes=await detector.detect(video);const code=codes.find(c=>/^(?:[0-9a-f]{2})+$/i.test(c.rawValue.trim()));if(code){await submit(code.rawValue,{'Content-Type':'text/plain'});return;}}catch(e){detector=null;}}
     {
      const scale=Math.min(1,1920/video.videoWidth);canvas.width=Math.round(video.videoWidth*scale);canvas.height=Math.round(video.videoHeight*scale);context.drawImage(video,0,0,canvas.width,canvas.height);
      const image=await new Promise(resolve=>canvas.toBlob(resolve,'image/jpeg',0.95));
      if(attempt!==generation||!stream)return;
      if(image){const form=new FormData();form.append('image',image,'camera.jpg');if(await submit(form,undefined,'scan'))return;}
     }
    }
   }catch(e){help.textContent=e.message+' Use a photo instead if scanning does not resume.';}
   if(attempt===generation&&stream&&!finished&&!submitted)setTimeout(scan,700);
  }
  scan();
 }catch(e){if(attempt===generation){stop();photoFallback.open=true;help.textContent='Live camera unavailable: '+e.message+'. Photograph the QR code below, then tap Send QR photo.';}}
};
})();
