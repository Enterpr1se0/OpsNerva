package webui

import (
	"os/exec"
	"testing"
)

func TestFileBrowserProgressAndRenderBounds(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for frontend state regression tests")
	}
	command := exec.Command(node, "--experimental-strip-types", "--input-type=module", "--eval", `
import assert from 'node:assert/strict';
import { FileTransferStore } from './src/features/sftp/transferStore.ts';
import { fileListWindow, fileListIndexes, fileListOverscan } from './src/components/fileListWindow.ts';

const timers=new Map();let timerID=0;
globalThis.setTimeout=(callback,delay)=>{assert.equal(delay,200);timers.set(++timerID,callback);return timerID;};
globalThis.clearTimeout=id=>timers.delete(id);
const flush=()=>{const callbacks=[...timers.values()];timers.clear();for(const callback of callbacks)callback();};
const store=new FileTransferStore();
const key='sftp:host';
const transfer=(loaded,name='file')=>({operation:'upload',name,loaded,total:20000});
const update=active=>store.set(key,{...store.record(key),active});
let states=0,progress=0,other=0;
store.subscribeState(key,()=>states++);
store.subscribeState('workspace:other',()=>other++);
let unsubscribe=store.subscribeProgress(key,()=>progress++);
update(transfer(0));assert.equal(states,1);assert.equal(progress,1);
const summary=store.state(key),snapshot=store.progress(key);
for(let index=1;index<=10000;index++)update(transfer(index));
assert.equal(states,1);assert.equal(other,0);assert.equal(progress,1);
assert.equal(store.state(key),summary);assert.equal(store.progress(key),snapshot);
assert.equal(timers.size,1);flush();
assert.equal(progress,2);assert.equal(store.progress(key).loaded,10000);assert.equal(timers.size,0);
// Even rapid file-name changes in a folder transfer use the same bounded stream.
for(let index=0;index<100;index++)update(transfer(10001+index,'file-'+index));
assert.equal(states,1);assert.equal(timers.size,1);flush();
assert.equal(store.progress(key).name,'file-99');
// Two visible consumers share one progress publication. Hiding the last one stops it.
const another=store.subscribeProgress(key,()=>{});
update(transfer(12000));unsubscribe();assert.equal(timers.size,1);another();assert.equal(timers.size,0);
for(let index=13000;index<14000;index++)update(transfer(index));
assert.equal(timers.size,0);assert.equal(states,1);
unsubscribe=store.subscribeProgress(key,()=>progress++);
assert.equal(store.progress(key).loaded,13999);
// Completion/cancellation is immediate and cancels a pending progress flush.
update(transfer(15000));assert.equal(timers.size,1);
store.set(key,{active:null,conflict:null,uploadVersion:1});
assert.equal(store.progress(key),null);assert.equal(store.state(key).operation,null);
assert.equal(store.state(key).uploadVersion,1);assert.equal(states,2);assert.equal(timers.size,0);
flush();assert.equal(store.progress(key),null);
// A subsequent run cannot receive the old run's delayed progress.
update(transfer(0,'new'));assert.equal(store.progress(key).name,'new');
update(transfer(1,'new'));store.dispose();assert.equal(timers.size,0);unsubscribe();

for(const rowHeight of [36,54]){
  for(const count of [0,1,10,10000,100000]){
    for(const height of [0,300,600,1200]){
      for(const scrollTop of [-10,0,1,5000,200000,count*rowHeight]){
        const window=fileListWindow(count,scrollTop,height,rowHeight);
        assert(window.start>=0&&window.start<=window.end&&window.end<=count);
        const focused=count?Math.floor(count/2):null;
        const indexes=fileListIndexes(window,focused,count);
        assert(indexes.length<=Math.ceil(height/rowHeight)+2*fileListOverscan+2);
        assert.equal(new Set(indexes).size,indexes.length);
        assert(indexes.every(index=>index>=0&&index<count));
        if(focused!==null)assert(indexes.includes(focused));
        if(count&&height){
          const top=Math.max(0,Math.min(scrollTop,Math.max(0,count*rowHeight-height)));
          assert(window.start*rowHeight<=top);
          assert(window.end*rowHeight>=Math.min(count*rowHeight,top+height));
        }
      }
    }
  }
}
// A directory shrinking while scrolled near its end must not render an empty window.
assert.deepEqual(fileListWindow(3,540000,600,54),{start:0,end:3});
assert.deepEqual(fileListIndexes({start:20,end:25},0,100),[0,20,21,22,23,24]);
console.log('10000 byte updates: 0 extra list notifications, 1 coalesced progress notification.');
console.log('100000 entries, 600px viewport: <= 26 SFTP rows / 31 Workspace rows, including focus retention.');
`)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("file browser state: %v\n%s", err, output)
	}
	t.Log(string(output))
}
