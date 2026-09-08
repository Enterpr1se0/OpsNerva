package webui

import (
	"os/exec"
	"testing"
)

// Run the real, dependency-free TypeScript state store with Node's type
// stripping. No browser, npm test framework, or repository-local artifacts.
func TestSFTPDeletionClientState(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for frontend state regression tests")
	}
	command := exec.Command(node, "--experimental-strip-types", "--input-type=module", "--eval", `
import assert from 'node:assert/strict';
import { SFTPDeletionStore, isSFTPDeletionActive } from './src/features/sftp/deletionStore.ts';
const job=(revision,status='running',host_id='host',id='job')=>({
  id,host_id,revision,status,path:'/tree',updated_at:'2026-09-07T00:00:00Z',
  progress:{discovered:10,removed:revision,failed:0,skipped:0,scan_complete:false,root_removed:status==='completed'},
});
const store=new SFTPDeletionStore();
const completions=[];
store.onComplete('host',(path,completed)=>completions.push({path,completed}));
let hostChanges=0,otherChanges=0,activeChanges=0,wasActive=false;
store.subscribe('host',()=>{
  hostChanges++;
  const active=isSFTPDeletionActive(store.record('host'));
  if(active!==wasActive){activeChanges++;wasActive=active;}
});
store.subscribe('other',()=>otherChanges++);
store.snapshot([]);
store.update(job(1));store.update(job(2));store.update(job(3));
assert.equal(hostChanges,3);assert.equal(otherChanges,0);assert.equal(activeChanges,1);
store.update(job(4,'completed'));
assert.equal(activeChanges,2);
assert.deepEqual(completions,[{path:'/tree',completed:true}]);
// A late DELETE response or queued pre-snapshot delta cannot revive a task.
store.update(job(1));store.snapshot([job(4,'completed')]);store.update(job(3));
assert.equal(store.record('host').status,'completed');assert.equal(completions.length,1);
const unsub=store.onComplete('host',()=>assert.fail('old completion replayed on mount'));
store.snapshot([job(4,'completed')]);unsub();
// The snapshot can lag an HTTP response from the same task.
store.snapshot([job(2)]);assert.equal(store.record('host').revision,4);
// Completion while the socket was disconnected still reconciles the directory.
store.update(job(5,'running','host','second'));
store.snapshot([job(6,'failed','host','second')]);
assert.deepEqual(completions.at(-1),{path:'/tree',completed:false});
assert.equal(completions.length,2);
// An initial historical result must not remove a newly recreated same-path file.
const fresh=new SFTPDeletionStore();let calls=0;
fresh.onComplete('host',()=>calls++);fresh.snapshot([job(9,'completed')]);assert.equal(calls,0);
// A very fast local task can finish before both the initial snapshot and HTTP reply.
const fast=new SFTPDeletionStore();let fastCalls=0;
fast.onComplete('host',()=>fastCalls++);fast.starting.add('host');fast.snapshot([job(3,'completed')]);
fast.update(job(1));fast.starting.delete('host');assert.equal(fastCalls,1);assert.equal(fast.record('host').status,'completed');
// Restart drops transient jobs; reconcile partial changes and accept new revisions.
store.update(job(7,'running','host','third'));store.snapshot([]);
assert.deepEqual(completions.at(-1),{path:'/tree',completed:false});
store.snapshot([job(1,'running','host','after-restart')]);store.update(job(2,'cancelled','host','after-restart'));
assert.equal(store.record('host').status,'cancelled');
// Snapshot omissions and delayed deltas do not restore an evicted historical task.
store.snapshot([job(100,'completed','other')]);store.update(job(90,'completed','evicted'));
assert.equal(store.record('evicted'),undefined);
for(let i=101;i<201;i++)store.update(job(i,'completed','host-'+i));
let retained=0;for(let i=101;i<201;i++)if(store.record('host-'+i))retained++;
assert.equal(retained,64);
`)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("SFTP deletion client state: %v\n%s", err, output)
	}
}
