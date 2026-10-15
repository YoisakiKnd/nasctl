// Geometry stays independent of the DOM so viewport changes cannot strand a window.
export function fitWindow(frame, width, height) {
  const w = Math.min(Math.max(360, frame.width), width);
  const h = Math.min(Math.max(260, frame.height), height);
  return {...frame, width:w, height:h, x:Math.min(Math.max(0, frame.x), Math.max(0,width-w)), y:Math.min(Math.max(0,frame.y),Math.max(0,height-h))};
}
export function newWindow(id, index, width, height, z) {
  return fitWindow({id, x:32+index*28, y:26+index*24, width:Math.min(1050,width-64), height:Math.min(660,height-52), z, minimized:false, maximized:false},width,height);
}
