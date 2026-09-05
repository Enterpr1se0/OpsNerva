import { FormEvent, useEffect, useLayoutEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import type { Terminal as XTermInstance } from '@xterm/xterm'
import { LoaderCircle, LockKeyhole, Power, Send, ShieldAlert, Square, TerminalSquare, X } from 'lucide-react'
import type { SSHShell, SSHShellEvent } from '../../../types'
import { api, sshShellWebSocketURL } from '../../../api/api'
import { AppSelect } from '../../../components/Controls'
import { PasswordInput } from '../../../components/PasswordInput'
import { writeClipboard } from '../../../lib/clipboard'
import { errorText } from '../../../lib/utils'
import { sshShellActive } from '../utils'
import { SSHHostStatusBar } from './SSHHostStatusBar'

export function SSHShellTerminal({initialShell,relatedShells=[],onSelect,onClose,onChanged,onError,embedded=false}:{initialShell:SSHShell;relatedShells?:SSHShell[];onSelect?:(shell:SSHShell)=>void;onClose:()=>void;onChanged:()=>void;onError:(message:string)=>void;embedded?:boolean}){
	const {t}=useTranslation()
	const [shell,setShell]=useState(initialShell)
	const [secret,setSecret]=useState('')
	const [sendingSecret,setSendingSecret]=useState(false)
	const [closing,setClosing]=useState(false)
	const terminalElement=useRef<HTMLDivElement>(null)
	const terminalRef=useRef<XTermInstance|null>(null)
	const socketRef=useRef<WebSocket|null>(null)
	const lastSequence=useRef(0)
	const onChangedRef=useRef(onChanged)
	const onErrorRef=useRef(onError)
	useLayoutEffect(()=>{onChangedRef.current=onChanged;onErrorRef.current=onError},[onChanged,onError])
	const active=sshShellActive(shell.status)

	useEffect(()=>{
		const container=terminalElement.current
		if(!container)return
		let disposed=false
		let cleanup=()=>{}
		void Promise.all([import('@xterm/xterm'),import('@xterm/addon-fit')]).then(([xtermModule,fitModule])=>{
			if(disposed)return
			const terminal=new xtermModule.Terminal({
				cursorBlink:true,
				convertEol:false,
				fontFamily:"'JetBrains Mono','Cascadia Code','SFMono-Regular',Consolas,monospace",
				fontSize:13,
				theme:{background:'#071019',foreground:'#d8e3ea',cursor:'#55d6be',selectionBackground:'#31546a'},
				scrollback:10_000,
			})
			const fit=new fitModule.FitAddon()
			terminal.loadAddon(fit)
			terminal.open(container)
			terminalRef.current=terminal
			terminal.attachCustomKeyEventHandler(event=>{
				if(event.type!=='keydown'||event.key.toLowerCase()!=='c'||!terminal.hasSelection())return true
				const copyShortcut=(event.metaKey||event.ctrlKey)&&!event.altKey
				if(!copyShortcut)return true
				event.preventDefault()
				void writeClipboard(terminal.getSelection()).catch(err=>onErrorRef.current(errorText(err))).finally(()=>terminal.focus())
				return false
			})
			let inputBuffer=''
			let inputTimer:number|undefined
			let reconnectTimer:number|undefined
			let terminalEnded=false
			let socket:WebSocket|null=null
			let outputFrame:number|undefined
			let outputBytes=0
			let outputChunks:Uint8Array[]=[]
			const outputEncoder=new TextEncoder()
			const flushOutput=()=>{
				outputFrame=undefined
				if(!outputBytes)return
				const combined=new Uint8Array(outputBytes)
				let offset=0
				for(const chunk of outputChunks){combined.set(chunk,offset);offset+=chunk.byteLength}
				outputChunks=[];outputBytes=0
				terminal.write(combined)
			}
			const queueOutput=(content:string|Uint8Array)=>{
				const chunk=typeof content==='string'?outputEncoder.encode(content):content
				if(!chunk.byteLength)return
				outputChunks.push(chunk);outputBytes+=chunk.byteLength
				if(outputFrame===undefined)outputFrame=requestAnimationFrame(flushOutput)
			}
			const sendCommand=(command:Record<string,unknown>)=>{
				if(socket?.readyState!==WebSocket.OPEN)return false
				socket.send(JSON.stringify(command))
				return true
			}
			const flushInput=()=>{
				if(inputTimer!==undefined)window.clearTimeout(inputTimer)
				inputTimer=undefined
				if(!inputBuffer)return
				const input=inputBuffer.slice(0,64<<10)
				if(!sendCommand({type:'input',content:input}))return
				inputBuffer=inputBuffer.slice(input.length)
				if(inputBuffer)inputTimer=window.setTimeout(flushInput,0)
			}
			const inputDisposable=terminal.onData(data=>{
				inputBuffer+=data
				if(data.includes('\r')||data.includes('\n')||data.includes('\x03'))flushInput()
				else if(inputTimer===undefined)inputTimer=window.setTimeout(flushInput,24)
			})
			let resizeTimer:number|undefined
			const resizeDisposable=terminal.onResize(({cols,rows})=>{
				if(resizeTimer!==undefined)window.clearTimeout(resizeTimer)
				resizeTimer=window.setTimeout(()=>{sendCommand({type:'resize',cols,rows})},80)
			})
			const observer=new ResizeObserver(()=>fit.fit())
			observer.observe(container)
			fit.fit()
			terminal.focus()

			const applyEvent=(event:SSHShellEvent)=>{
				if(event.sequence<=lastSequence.current)return
				lastSequence.current=event.sequence
				if(event.content&&(event.stream==='stdout'||event.stream==='stderr'))queueOutput(event.content)
				if(event.status&&event.stream==='status'){
					setShell(current=>({...current,status:event.status as SSHShell['status'],last_sequence:event.sequence}))
					if(!sshShellActive(event.status)){
						terminalEnded=true
						socket?.close()
						void api.sshShell(initialShell.id,event.sequence).then(snapshot=>setShell(snapshot.shell)).catch(()=>{/* final state is already visible */})
						onChangedRef.current()
					}
				}
			}
			const connect=()=>{
				if(disposed||terminalEnded)return
				socket=new WebSocket(sshShellWebSocketURL(initialShell.id,lastSequence.current))
				socket.binaryType='arraybuffer'
				socketRef.current=socket
				socket.onopen=()=>{
					sendCommand({type:'resize',cols:terminal.cols,rows:terminal.rows})
					flushInput()
				}
				socket.onmessage=message=>{
					try{
						if(message.data instanceof ArrayBuffer){
							if(message.data.byteLength<10)return
							const view=new DataView(message.data)
							if(view.getUint8(0)!==1)return
							const sequence=Number(view.getBigUint64(2))
							if(sequence<=lastSequence.current)return
							lastSequence.current=sequence
							queueOutput(new Uint8Array(message.data,10))
							return
						}
						const payload=JSON.parse(String(message.data)) as {type:string;event?:SSHShellEvent;error?:string}
						if(payload.type==='event'&&payload.event)applyEvent(payload.event)
						else if(payload.type==='error'&&payload.error)onErrorRef.current(payload.error)
					}catch{/* malformed frames are ignored; reconnect resumes from the last valid sequence */}
				}
				socket.onclose=()=>{
					if(socketRef.current===socket)socketRef.current=null
					if(!disposed&&!terminalEnded)reconnectTimer=window.setTimeout(connect,1000)
				}
			}
			connect()
			cleanup=()=>{
				flushInput()
				if(outputFrame!==undefined)cancelAnimationFrame(outputFrame)
				flushOutput()
				terminalEnded=true
				if(reconnectTimer!==undefined)window.clearTimeout(reconnectTimer)
				socket?.close()
				if(socketRef.current===socket)socketRef.current=null
				observer.disconnect()
				inputDisposable.dispose()
				resizeDisposable.dispose()
				if(inputTimer!==undefined)window.clearTimeout(inputTimer)
				if(resizeTimer!==undefined)window.clearTimeout(resizeTimer)
				terminal.dispose()
				terminalRef.current=null
			}
			if(disposed)cleanup()
		}).catch(err=>onErrorRef.current(errorText(err)))
		return()=>{disposed=true;cleanup()}
	},[initialShell.id,t])

	const sendSecret=async(event:FormEvent)=>{
		event.preventDefault()
		if(!secret||!active||sendingSecret)return
		setSendingSecret(true)
		try{
			const socket=socketRef.current
			if(!socket||socket.readyState!==WebSocket.OPEN)throw new Error(t('sshShell.streamEnded'))
			socket.send(JSON.stringify({type:'input',content:`${secret}\r`,sensitive:true}));setSecret('');terminalRef.current?.focus()
		}
		catch(err){onError(errorText(err))}
		finally{setSendingSecret(false)}
	}
	const interrupt=async()=>{try{const socket=socketRef.current;if(!socket||socket.readyState!==WebSocket.OPEN)throw new Error(t('sshShell.streamEnded'));socket.send(JSON.stringify({type:'interrupt'}))}catch(err){onError(errorText(err))}finally{terminalRef.current?.focus()}}
	const stop=async()=>{setClosing(true);try{setShell(await api.closeSSHShell(shell.id));onChanged();onClose()}catch(err){onError(errorText(err))}finally{setClosing(false)}}
	const titleID=`ssh-shell-terminal-title-${shell.id}`
	const workspaceShell=shell.kind==='workspace'
	const managedShell=['agent','mcp','workspace_agent'].includes(shell.surface)
		const terminal=<section className={`ssh-shell-terminal-dialog ${embedded?'embedded':''} ${workspaceShell?'':'monitored'}`} role={embedded?undefined:'dialog'} aria-modal={embedded?undefined:true} aria-labelledby={embedded?undefined:titleID}>
			{!embedded&&<header>
				<div><TerminalSquare size={20}/><span>{workspaceShell&&<small>{t('workspace.terminal')}</small>}<h2 id={titleID}>{workspaceShell?shell.workspace_id:shell.host_name||shell.host_id}</h2></span></div>
				<div className="ssh-shell-terminal-state">{workspaceShell&&relatedShells.length>1&&<AppSelect className="terminal-session-select" value={shell.id} ariaLabel={t('workspace.switchTerminal')} onChange={value=>{const selected=relatedShells.find(item=>item.id===value);if(selected)onSelect?.(selected)}} options={relatedShells.map(item=>({value:item.id,label:`${t(item.surface==='workspace_agent'?'workspace.agent':'workspace.operator')} · ${item.cwd||'.'}`}))}/>}<em className={shell.status}>{t(`statusLabels.${shell.status}`,{defaultValue:shell.status})}</em><code>{shell.elevated?'root':shell.user}</code>{!embedded&&<button type="button" onClick={onClose} title={t('common.close')}><X size={16}/></button>}</div>
			</header>}
			<div className="ssh-shell-terminal-screen" ref={terminalElement}/>
			{!workspaceShell&&<SSHHostStatusBar shell={shell}/>}
			<footer>
				{managedShell&&<form onSubmit={sendSecret}><LockKeyhole size={14}/><PasswordInput value={secret} onChange={event=>setSecret(event.target.value)} disabled={!active||sendingSecret} placeholder={t('sshShell.sensitivePlaceholder')} autoComplete="off"/><button className="primary" disabled={!secret||!active||sendingSecret}>{sendingSecret?<LoaderCircle className="spin" size={13}/>:<Send size={13}/>} {t('sshShell.sendSensitive')}</button></form>}
				<div><button type="button" disabled={!active} onClick={()=>void interrupt()}><Square size={10}/>{t('sshShell.interrupt')}</button><button type="button" className="danger" disabled={!active||closing} onClick={()=>void stop()}>{closing?<LoaderCircle className="spin" size={13}/>:<Power size={13}/>} {t('sshShell.closeSession')}</button></div>
			</footer>
			{shell.error&&<p className="ssh-shell-terminal-error"><ShieldAlert size={13}/>{shell.error}</p>}
		</section>
	return embedded?terminal:<div className="ssh-shell-terminal-backdrop">{terminal}</div>
}
